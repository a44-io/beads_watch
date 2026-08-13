package brexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBr writes a stand-in for the br binary that reports what it was handed,
// so we can assert on the environment and argv br would actually have seen.
func fakeBr(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "br")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHostileEnvIsScrubbed is the regression test for the failure mode this
// daemon exists to avoid: BEADS_DIR outranks cwd in br's workspace
// resolution, so an inherited value would serve every repo from one workspace
// while still reporting the requested repo's name.
func TestHostileEnvIsScrubbed(t *testing.T) {
	br := fakeBr(t, `env | grep -E '^(BEADS_DIR|BD_DB|BD_DATABASE|BD_ACTOR|BR_OUTPUT_FORMAT|TOON_DEFAULT_FORMAT)=' || true`)

	for _, name := range []string{"BEADS_DIR", "BD_DB", "BD_DATABASE", "BR_OUTPUT_FORMAT", "TOON_DEFAULT_FORMAT"} {
		t.Setenv(name, "/poisoned")
	}

	r := &Runner{Path: br}
	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"where"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "" {
		t.Errorf("br saw workspace-overriding env vars it must never see:\n%s", got)
	}
}

func TestActorIsSetPerRequest(t *testing.T) {
	br := fakeBr(t, `printf '%s' "$BD_ACTOR"`)
	// An inherited BD_ACTOR must not leak through as the caller's identity.
	t.Setenv("BD_ACTOR", "whoever-started-the-daemon")

	r := &Runner{Path: br}

	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"x"}, Actor: "dev"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != "dev" {
		t.Errorf("BD_ACTOR = %q, want %q", got, "dev")
	}

	// With no actor resolved, br must see nothing rather than an inherited name.
	res, err = r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != "" {
		t.Errorf("BD_ACTOR = %q, want empty when identity is unknown", got)
	}
}

func TestRunsInRequestedDir(t *testing.T) {
	br := fakeBr(t, `pwd -P`)
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	r := &Runner{Path: br}
	res, err := r.Run(context.Background(), Request{Dir: dir, Args: []string{"where"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != real {
		t.Errorf("cwd = %q, want %q", got, real)
	}
}

// TestExitCodePassthrough covers br's typed exit codes. Collapsing these into
// a generic failure would erase the difference between not-found, validation,
// and dependency-cycle.
func TestExitCodePassthrough(t *testing.T) {
	for _, code := range []int{0, 2, 3, 4, 5, 6, 7, 8} {
		br := fakeBr(t, `echo '{"error":{}}'; exit `+itoa(code))
		r := &Runner{Path: br}
		res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"show"}})
		if err != nil {
			t.Fatalf("exit %d: Run returned error %v; a nonzero br exit is data, not a daemon failure", code, err)
		}
		if res.ExitCode != code {
			t.Errorf("ExitCode = %d, want %d", res.ExitCode, code)
		}
	}
}

func TestStdoutIsVerbatim(t *testing.T) {
	// Partial-batch failures emit two concatenated JSON documents. The daemon
	// must not reformat, re-encode, or split them.
	payload := `{"closed":[{"id":"a"}]}` + "\n" + `{"error":{"code":"CLOSE_INCOMPLETE"}}` + "\n"
	br := fakeBr(t, `printf '%s' '`+payload+`'; exit 3`)

	r := &Runner{Path: br}
	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"close"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != payload {
		t.Errorf("stdout = %q, want byte-identical %q", res.Stdout, payload)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestTimeoutReported(t *testing.T) {
	br := fakeBr(t, `sleep 5`)
	r := &Runner{Path: br}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := r.Run(ctx, Request{Dir: t.TempDir(), Args: []string{"ready"}})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

func TestOutputIsCapped(t *testing.T) {
	br := fakeBr(t, `head -c 100000 /dev/zero | tr '\0' 'x'`)
	r := &Runner{Path: br, MaxOutput: 1000}

	res, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"list"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stdout) != 1000 {
		t.Errorf("len(stdout) = %d, want 1000", len(res.Stdout))
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true so the caller is not misled")
	}
}

func TestMissingBinaryIsDaemonError(t *testing.T) {
	r := &Runner{Path: filepath.Join(t.TempDir(), "does-not-exist")}
	if _, err := r.Run(context.Background(), Request{Dir: t.TempDir(), Args: []string{"ready"}}); err == nil {
		t.Error("want an error when br cannot be executed")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
