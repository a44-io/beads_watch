// Package brexec runs the br binary and reports exactly what it said.
//
// The daemon deliberately knows nothing about what a bead is. It never parses
// br's output, never caches it, and never models issues. That is the whole
// design: if we never reimplement br's semantics, they cannot drift from it.
package brexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrTimeout means br was still running when the deadline expired.
var ErrTimeout = errors.New("br timed out")

// hostileEnv lists environment variables that would silently retarget br at a
// different workspace, or change the shape of its output, regardless of the
// repo named in the request URL.
//
// This is not hypothetical. BEADS_DIR sits ABOVE cwd-walking in br's
// resolution order, so a daemon started from a shell that exports it would
// serve every repo's data from one workspace while cheerfully reporting the
// requested repo's name. The bug is invisible from the outside: you get a
// valid, well-formed, completely wrong answer.
//
// BD_ACTOR is scrubbed because the daemon sets it per request from the
// caller's forwarded identity; an inherited value would misattribute the
// audit trail to whoever happened to start the process.
var hostileEnv = []string{
	"BEADS_DIR",
	"BD_DB",
	"BD_DATABASE",
	"BD_ACTOR",
	"BR_OUTPUT_FORMAT",
	"TOON_DEFAULT_FORMAT",
}

// Request is one br invocation, already validated by the caller.
type Request struct {
	// Dir is the repo working directory. br resolves its workspace by walking
	// up from here, which is what makes .beads/redirect files work.
	Dir string
	// Args is the full argv after the binary name.
	Args []string
	// Actor becomes BD_ACTOR, or is left unset when identity is unknown.
	Actor string
}

// Result is everything the daemon learned from one br invocation.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	// ExitCode is br's own typed exit code, passed through untouched:
	// 0 ok, 2 database, 3 not-found, 4 validation, 5 cycle, 6 sync, 7 config, 8 I/O.
	ExitCode int
	Duration time.Duration
}

// Runner executes br. It is safe for concurrent use.
type Runner struct {
	// Path is the br binary. Empty means look it up on PATH.
	Path string
	// MaxOutput caps how many bytes of each stream we retain in memory, so a
	// pathological command cannot exhaust the daemon.
	MaxOutput int
}

// Run executes br and returns its result. The returned error is non-nil only
// for daemon-level failures — br refusing to start, or exceeding the deadline.
// A nonzero br exit code is a successful Run with a nonzero Result.ExitCode:
// br failing is data, not a daemon error.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	bin := r.Path
	if bin == "" {
		bin = "br"
	}
	max := r.MaxOutput
	if max <= 0 {
		max = 8 << 20
	}

	cmd := exec.CommandContext(ctx, bin, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = r.childEnv(req.Actor)
	// br must never see a terminal or block waiting on input.
	cmd.Stdin = nil

	var stdout, stderr capped
	stdout.limit = max
	stderr.limit = max
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := &Result{
		Stdout:          stdout.bytes(),
		Stderr:          stderr.bytes(),
		StdoutTruncated: stdout.truncated(),
		StderrTruncated: stderr.truncated(),
		Duration:        elapsed,
	}

	switch {
	case err == nil:
		res.ExitCode = 0
		return res, nil
	case ctx.Err() != nil:
		// CommandContext kills the process on deadline; report that plainly
		// rather than passing off the resulting signal as a br exit code.
		return res, ErrTimeout
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
}

// childEnv builds br's environment: the daemon's own, minus anything that
// would override the workspace or output shape, plus our per-request actor.
func (r *Runner) childEnv(actor string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || isHostile(name) {
			continue
		}
		env = append(env, kv)
	}
	// Keep br's stderr to real diagnostics; RUST_LOG=debug emits megabytes per
	// invocation and would swamp the response headers.
	env = append(env, "RUST_LOG=error")
	if actor != "" {
		env = append(env, "BD_ACTOR="+actor)
	}
	return env
}

func isHostile(name string) bool {
	for _, h := range hostileEnv {
		if name == h {
			return true
		}
	}
	// RUST_LOG is re-set unconditionally below; drop any inherited value so we
	// do not end up with two definitions.
	return name == "RUST_LOG"
}

// capped is an io.Writer that retains at most limit bytes but keeps counting,
// so we can tell the caller honestly that output was cut short.
type capped struct {
	buf   []byte
	limit int
	seen  int
}

func (c *capped) Write(p []byte) (int, error) {
	c.seen += len(p)
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil
}

func (c *capped) bytes() []byte { return c.buf }

func (c *capped) truncated() bool { return c.seen > len(c.buf) }
