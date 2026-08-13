package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func bodyWithActor(t *testing.T, actor string) string {
	t.Helper()
	// Built with json.Marshal so the control characters under test are
	// properly escaped on the wire and reach validActor decoded, rather than
	// being rejected earlier as malformed JSON.
	b, err := json.Marshal(map[string]any{
		"args":  []string{"create", "x"},
		"actor": actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestActorCannotCorruptAuditTrail guards the record, not the response: Go
// already strips CR and LF from response header values, but BD_ACTOR is also
// written verbatim into .beads JSONL and audit events, where an embedded
// newline would damage the trail this daemon exists to keep honest.
func TestActorCannotCorruptAuditTrail(t *testing.T) {
	s, _ := testServer(t, `printf '%s' "$BD_ACTOR"`)

	bad := []string{
		"a\r\nX-Evil: 1",
		"a\nb",
		"a\tb",
		"a\x00b",
		"a\x07b",
		strings.Repeat("a", maxActorLen+1),
	}
	for _, actor := range bad {
		rec := post(t, s, "demo", bodyWithActor(t, actor))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("actor %q: status = %d, want 400", actor, rec.Code)
		}
	}

	for _, actor := range []string{"laptop-2", "someone@example.com@phone", "dev"} {
		rec := post(t, s, "demo", bodyWithActor(t, actor))
		if rec.Code != http.StatusOK {
			t.Errorf("actor %q rejected: status = %d, want 200", actor, rec.Code)
		}
		if got := rec.Body.String(); got != actor {
			t.Errorf("BD_ACTOR = %q, want %q", got, actor)
		}
	}
}
