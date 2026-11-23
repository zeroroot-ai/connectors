package main

import (
	"net/http"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/kit/kittest"
)

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
		{"start_patch_deployment", map[string]any{"entra_device_id": devID, "catalog_entry_id": entryID, "kind": "quality", "restart_allowed": true, "days_until_forced_reboot": 2}, kit.ClassOutcomeUnknown},
		{"pause_patch_deployment", map[string]any{"deployment_id": depID}, kit.ClassUnavailable},
		{"resume_patch_deployment", map[string]any{"deployment_id": depID}, kit.ClassUnavailable},
		{"remove_patch_deployment", map[string]any{"deployment_id": depID}, kit.ClassUnavailable},
	} {
		_, call, _ := setup(t)
		call(c.tool, c.args)
		if got := kittest.ErrorClass(t, kittest.LastResult()); got.Class != c.want {
			t.Errorf("%s %v: class = %s, want %s", c.tool, c.args, got.Class, c.want)
		}
	}
}
