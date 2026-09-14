package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"beads_watch/internal/events"
)

// TestMissingRepoPathIsNotFatal is the fix for the crash loop: one archived
// checkout must not take every healthy repo offline with it.
func TestMissingRepoPathIsNotFatal(t *testing.T) {
	good, ghost := mixedRepos(t)
	cfg := &Config{Repos: []Repo{good, ghost}}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() = %v, want a missing path to degrade rather than fail", err)
	}

	g, _ := cfg.Lookup("good")
	if g.Err() != nil {
		t.Errorf("good.Err() = %v, want nil", g.Err())
	}
	gh, _ := cfg.Lookup("ghost")
	if gh.Err() == nil || !strings.Contains(gh.Err().Error(), "no such file") {
		t.Errorf("ghost.Err() = %v, want the stat error", gh.Err())
	}
	if got := cfg.Unavailable(); len(got) != 1 || got[0].Name != "ghost" {
		t.Errorf("Unavailable() = %v, want just ghost", got)
	}
}

func TestFileWhereDirectoryExpectedIsNotFatal(t *testing.T) {
	good, _ := mixedRepos(t)
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Repos: []Repo{good, {Name: "flat", Path: file}}}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() = %v, want a non-directory to degrade rather than fail", err)
	}
	flat, _ := cfg.Lookup("flat")
	if flat.Err() == nil || !strings.Contains(flat.Err().Error(), "not a directory") {
		t.Errorf("flat.Err() = %v, want 'not a directory'", flat.Err())
	}
}

// TestNoServableRepoIsFatal guards the other edge: a daemon that comes up
// and serves nothing is worse than one that refuses to start.
func TestNoServableRepoIsFatal(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Repos: []Repo{
		{Name: "a", Path: filepath.Join(dir, "a")},
		{Name: "b", Path: filepath.Join(dir, "b")},
	}}
	err := cfg.Normalize()
	if err == nil {
		t.Fatal("Normalize() = nil, want an error when every path is missing")
	}
	// Name every casualty: the operator has to fix each one.
	for _, want := range []string{"no servable repos", `"a"`, `"b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestConfigShapedErrorsStayFatal is the regression guard against turning
// this fix into "startup never fails". These are typos; retrying cannot help.
func TestConfigShapedErrorsStayFatal(t *testing.T) {
	good, _ := mixedRepos(t)
	state := filepath.Join(t.TempDir(), "cursor.json")

	cases := map[string]*Config{
		"empty path":     {Repos: []Repo{good, {Name: "x", Path: ""}}},
		"unsafe name":    {Repos: []Repo{good, {Name: "../x", Path: good.Path}}},
		"duplicate name": {Repos: []Repo{good, {Name: "good", Path: good.Path}}},
		"notify names an unserved repo": {
			Repos: []Repo{good},
			Notify: &events.Config{
				URL: "http://ntfy.test", Topic: "t", Node: "n", StateFile: state,
				Repos: []string{"nope"},
			},
		},
	}
	for name, cfg := range cases {
		if err := cfg.Normalize(); err == nil {
			t.Errorf("%s: Normalize() = nil, want a fatal config error", name)
		}
	}
}

func TestNotifyReposOmitsUnavailable(t *testing.T) {
	good, ghost := mixedRepos(t)
	state := filepath.Join(t.TempDir(), "cursor.json")
	cfg := &Config{
		Repos:  []Repo{good, ghost},
		Notify: &events.Config{URL: "http://ntfy.test", Topic: "t", Node: "n", StateFile: state},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	got := cfg.NotifyRepos()
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("NotifyRepos() = %v, want only the repo that is there to tail", got)
	}

	// Naming the ghost explicitly is still not a config error — it is served,
	// just not here right now — and still not tailed.
	cfg.Notify.Repos = []string{"good", "ghost"}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize() = %v, want naming an unavailable repo to be allowed", err)
	}
	if got := cfg.NotifyRepos(); len(got) != 1 || got[0].Name != "good" {
		t.Errorf("NotifyRepos() = %v, want ghost omitted even when named", got)
	}
}
