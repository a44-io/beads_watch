// Command beads_watch serves the br CLI over HTTP on a unix socket, so any
// machine on the tailnet can read and write beads through `tailscale serve`
// without ssh.
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
		listenAddr  = flag.String("listen", "", "listen on a TCP address (e.g. 127.0.0.1:7717) instead of a unix socket")
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

	cfg, err := loadConfig(*configPath, repos)
	if err != nil {
		return err
	}
	if *socketPath != "" {
		cfg.Socket = *socketPath
	}
	if *timeout > 0 {
		cfg.TimeoutSeconds = *timeout
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}

	if *printConfig {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A unix socket is the preferred transport: mode 0600 keeps every other
	// local user out, which matters on a box running agent swarms. TCP exists
	// because `tailscale serve` needs root to proxy a unix socket but not a
	// localhost port; the cost is that any local process can reach it.
	var (
		ln   net.Listener
		addr string
	)
	if *listenAddr != "" {
		if ln, err = net.Listen("tcp", *listenAddr); err != nil {
			return err
		}
		addr = *listenAddr
	} else {
		if ln, err = listenSocket(cfg.Socket); err != nil {
			return err
		}
		addr = cfg.Socket
		defer os.Remove(cfg.Socket)
	}

	srv := &http.Server{
		Handler:           server.New(cfg, log).Handler(),
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

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	names := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		names[i] = r.Name
	}
	log.Info("serving", "addr", addr, "repos", strings.Join(names, ","), "version", server.Version)

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
// the default config path.
func loadConfig(path string, repos repoFlag) (*server.Config, error) {
	if len(repos) > 0 {
		return &server.Config{Repos: []server.Repo(repos)}, nil
	}
	if path == "" {
		path = server.DefaultConfigPath()
		if path == "" {
			return nil, errors.New("no config file and no --repo flags; nothing to serve")
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s and no --repo flags; nothing to serve", path)
		}
	}
	return server.LoadConfig(path)
}

// listenSocket binds the unix socket, refusing to steal it from a live daemon
// but clearing it when the previous process died without cleaning up.
func listenSocket(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		conn, derr := net.DialTimeout("unix", path, time.Second)
		if derr == nil {
			conn.Close()
			return nil, fmt.Errorf("another beads_watch is already listening on %s", path)
		}
		// Nothing accepting: a stale socket from a killed process.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// tailscaled proxies to this socket as root, which is unaffected by mode
	// bits; 0600 keeps every other local user out.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
