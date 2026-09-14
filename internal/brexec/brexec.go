// Package brexec runs the br binary and reports exactly what it said.
//
// The daemon deliberately knows nothing about what a bead is. It never parses
// br's output, never caches it, and never models issues. That is the whole
// design: if we never reimplement br's semantics, they cannot drift from it.
package brexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
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

// maxExcerpt bounds the one-line diagnostics below. They render inline
// beside a repo name in whatever consumer shows them, so they are elided,
// not wrapped.
const maxExcerpt = 200

// ErrorMessage is what a failed invocation said, as one line for a log or a
// status field. br writes its error envelope — {"error":{"code","message",
// "hint",…}} — pretty-printed to stdout, so the first line of that stream is
// a bare brace and says nothing; when an envelope is there its message is
// relayed, with the hint appended. This is the one place br's output is read
// rather than passed through, and it reads a contract without depending on
// it: anything that does not fit the shape degrades to the plain-text path,
// never to an empty string.
func (r *Result) ErrorMessage() string {
	for _, s := range [][]byte{r.Stdout, r.Stderr} {
		if msg := envelopeMessage(s); msg != "" {
			return msg
		}
	}
	// stderr first: that is where a plain-text failure — a usage error, a
	// panic, sqlite3's "Error: …" — lands, while stdout at that point is
	// partial output at best.
	return Excerpt(r.Stderr, r.Stdout)
}

func envelopeMessage(s []byte) string {
	// json.Valid first, so a text stream that happens to start with a brace
	// never reaches the decoder.
	if !json.Valid(s) {
		return ""
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(s, &env); err != nil {
		return ""
	}
	msg := strings.TrimSpace(env.Error.Message)
	if msg == "" {
		return ""
	}
	if hint := strings.TrimSpace(env.Error.Hint); hint != "" {
		msg += " (hint: " + hint + ")"
	}
	return elide(msg)
}

// Excerpt is the first non-empty stream, in the order given, as one line:
// newlines are joined with " / " so a multi-line message still says
// something, and the result is elided at maxExcerpt bytes. "no output" when
// every stream is empty.
func Excerpt(streams ...[]byte) string {
	for _, s := range streams {
		t := strings.TrimSpace(string(s))
		if t == "" {
			continue
		}
		var lines []string
		for _, l := range strings.Split(t, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return elide(strings.Join(lines, " / "))
	}
	return "no output"
}

func elide(s string) string {
	if len(s) <= maxExcerpt {
		return s
	}
	cut := maxExcerpt
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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
