package events

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Normalize fills defaults, trims the URL, and reads token_file into Token.
// One whole-struct comparison, so a new default cannot land without the
// test naming it; Node and StateFile are pinned in the input because their
// defaults come from the host and from XDG, not from the code under test.
func TestConfigNormalize(t *testing.T) {
	// The file's content is a marker, not a credential: the assertion below is
	// that Normalize read it back, trailing newline trimmed.
	marker := "read-from-file"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "cursor.json")

	got := &Config{URL: "https://ntfy.example.com/", Topic: "beads-events", TokenFile: tokenFile, Node: "testnode", StateFile: state}
	if err := got.Normalize(); err != nil {
		t.Fatal(err)
	}

	yes := true
	want := &Config{
		URL:              "https://ntfy.example.com",
		Topic:            "beads-events",
		Token:            marker,
		TokenFile:        tokenFile,
		Node:             "testnode",
		PollMS:           1000,
		StateFile:        state,
		RouteAssignments: &yes,
		AgentTopicPrefix: "agent-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Normalize()\n got %+v\nwant %+v", got, want)
	}

	for _, bad := range []Config{
		{Topic: "beads"}, // no url
		{URL: "ntfy.example.com", Topic: "beads"}, // no scheme
		{URL: "https://x", Topic: "no spaces!"},   // bad topic
		{URL: "https://x", Topic: "b", TokenFile: "/nonexistent-path-xyz"},
	} {
		bad := bad
		if err := bad.Normalize(); err == nil {
			t.Errorf("Normalize(%+v) should have failed", bad)
		}
	}
}
