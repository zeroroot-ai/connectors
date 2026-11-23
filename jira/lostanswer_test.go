package main

import (
	"net/http"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

// wrap lets a test put a layer around the handler of the vendor mock.
var wrap = func(h http.Handler) http.Handler { return h }

// The answer of each call that changes the vendor is lost: the connection closes
// with no answer. A request that the client does not repeat may have run on the
// vendor, so the class is outcome_unknown. A request that the tool declared
// idempotent is repeated by the client, so the class is unavailable.
func TestLostAnswerOfEachWriteTool(t *testing.T) {
	wrap = kittest.LostAnswer
	t.Cleanup(func() { wrap = func(h http.Handler) http.Handler { return h } })
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"create_ticket", map[string]any{"title": "a", "description": "b", "queue": "SEC"}, kit.ClassOutcomeUnknown},
		{"create_ticket", map[string]any{"title": "a", "description": "b", "queue": "SEC", "dedup_key": "finding/42"}, kit.ClassUnavailable},
		{"update_ticket", map[string]any{"ticket_id": key, "title": "x"}, kit.ClassUnavailable},
		{"add_comment", map[string]any{"ticket_id": key, "body": "checked"}, kit.ClassOutcomeUnknown},
		{"close_ticket", map[string]any{"ticket_id": key, "resolution": "patched"}, kit.ClassOutcomeUnknown},
		{"transition_ticket", map[string]any{"ticket_id": key, "transition_id": "11"}, kit.ClassOutcomeUnknown},
	} {
		_, call, _ := setup(t)
		call(c.tool, c.args)
		if got := kittest.ErrorClass(t, kittest.LastResult()); got.Class != c.want {
			t.Errorf("%s %v: class = %s, want %s", c.tool, c.args, got.Class, c.want)
		}
	}
}
