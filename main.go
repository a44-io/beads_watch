// Command beads_watch serves the br CLI over HTTP — on a unix socket for
// local callers and on this box's tailnet address for the reverse proxy — so
// any machine on the tailnet can read and write beads without ssh.
//
// It is a pipe to br. It does not model issues, cache results, or maintain a
// projection — so `br ready` over HTTP is correct by construction, and a br
// upgrade adds features here for free.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"beads_watch/internal/events"
	"beads_watch/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "beads_watch: "+err.Error())
		os.Exit(1)
	}
}

type repoFlag []server.Repo

func (r *repoFlag) String() string { return "" }

func (r *repoFlag) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok {
		// Bare path: name it after its directory.
		path, name = v, filepath.Base(filepath.Clean(v))
	}
	if path == "" {
		return fmt.Errorf("missing path in %q", v)
	}
	*r = append(*r, server.Repo{Name: name, Path: path})
	return nil
}

func run() error {
	var (
		configPath  = flag.String("config", "", "config file (default "+server.DefaultConfigPath()+")")
		socketPath  = flag.String("socket", "", "unix socket to listen on (overrides config)")
		listenAddr  = flag.String("listen", "", "TCP address to listen on as well, e.g. 127.0.0.1:7717 (overrides config; without a socket this is TCP only)")
		timeout     = flag.Int("timeout", 0, "per-request br timeout in seconds (overrides config)")
		printConfig = flag.Bool("print-config", false, "print the effective config and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		repos       repoFlag
	)
	flag.Var(&repos, "repo", "serve a repo as name=path (repeatable; overrides config)")
	flag.Parse()

	if *showVersion {
		fmt.Println("beads_watch " + server.Version)
		// Machine-read by scripts/publish-dist.sh and setup.sh, which compare
		// this against the commit they are about to publish or install. Printed
		// only when stamped, so an unstamped source build stays silent about a
		// commit it does not know.
		if server.Commit != "" {
			fmt.Println("commit: " + server.Commit)
		}
		if server.BuiltAt != "" {
			fmt.Println("built:  " + server.BuiltAt)
		}
		return nil
	}

	cfg, source, err := loadConfig(*configPath, repos)
	if err != nil {
		return err
	}
	if *socketPath != "" {
		cfg.Socket = *socketPath
	}
	if *listenAddr != "" {
		cfg.Listen = *listenAddr
	}
	if *timeout > 0 {
		cfg.TimeoutSeconds = *timeout
	}
	// Once, after every override: the socket default depends on whether a
	// TCP address ended up set, so this cannot happen inside LoadConfig.
	if err := cfg.Normalize(); err != nil {
		if source != "" {
			return fmt.Errorf("%s: %w", source, err)
		}
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A repo whose path did not stat is served in name only: GET /v1/repos
	// lists it as ok:false, POST /v1/repos/{repo}/br answers 503, and every
	// request re-probes it so a later clone heals it without a restart. One
	// line per repo, once, so the journal explains the gap without becoming
	// the crash loop this replaced. Emitted before --print-config too, since
	// that is how an operator checks a config before wiring systemd.
	unavailable := cfg.Unavailable()
	for _, r := range unavailable {
		log.Warn("repo path unusable, serving it as unavailable",
			"repo", r.Name, "path", r.Path, "err", r.Err())
	}

	if *printConfig {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}

	// Two listeners, one server. The unix socket is the local path: mode 0600
	// keeps every other local user out, which matters on a box running agent
	// swarms. The TCP address is the tailnet path: the reverse proxy reaches
	// it directly, and because the daemon owns the socket it sees the real
	// peer, which is what lets it tell the proxy from anyone else.
	var (
		lns   []net.Listener
		addrs []string
	)
	if cfg.Socket != "" {
		ln, err := server.ListenSocket(cfg.Socket)
		if err != nil {
			return err
		}
		defer os.Remove(cfg.Socket)
		lns, addrs = append(lns, ln), append(addrs, cfg.Socket)
	}
	if cfg.Listen != "" {
		ln, err := server.ListenTCP(cfg.Listen)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return err
		}
		lns, addrs = append(lns, ln), append(addrs, cfg.Listen)
	}

	handler := server.New(cfg, log)
	srv := &http.Server{
		Handler:           handler.Handler(),
		ConnContext:       handler.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Notify != nil {
		watched := cfg.NotifyRepos()
		if _, err := events.Start(ctx, log, cfg.Notify, watched, cfg.BrPath); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		log.Info("notifying", "url", cfg.Notify.URL, "topic", cfg.Notify.Topic,
			"node", cfg.Notify.Node, "repos", len(watched))
	}

	// One error slot per listener: Serve returns on each when Shutdown closes
	// them, and the buffer keeps the late ones from blocking a goroutine.
	errCh := make(chan error, len(lns))
	for _, ln := range lns {
		go func(ln net.Listener) {
			errCh <- srv.Serve(ln)
		}(ln)
	}

	names := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		names[i] = r.Name
	}
	attrs := []any{"addr", strings.Join(addrs, ","), "repos", strings.Join(names, ","), "version", server.Version}
	if len(cfg.TrustedProxies) > 0 {
		attrs = append(attrs, "trusted_proxies", strings.Join(cfg.TrustedProxies, ","))
	}
	if len(unavailable) > 0 {
		bad := make([]string, len(unavailable))
		for i, r := range unavailable {
			bad[i] = r.Name
		}
		attrs = append(attrs, "unavailable", strings.Join(bad, ","))
	}
	log.Info("serving", attrs...)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// loadConfig prefers explicit --repo flags, then the named config file, then
// the default config path. source names the file the config came from, or is
// empty for --repo, so a validation error can say which file it is about.
func loadConfig(path string, repos repoFlag) (cfg *server.Config, source string, err error) {
	if len(repos) > 0 {
		return &server.Config{Repos: []server.Repo(repos)}, "", nil
	}
	if path == "" {
		path = server.DefaultConfigPath()
		if path == "" {
			return nil, "", errors.New("no config file and no --repo flags; nothing to serve")
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("no config at %s and no --repo flags; nothing to serve", path)
		}
	}
	cfg, err = server.LoadConfig(path)
	return cfg, path, err
}
