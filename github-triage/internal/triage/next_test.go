package triage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextNamesRemainingTools(t *testing.T) {
	c := testConfig(t)
	c.StatePath = filepath.Join(t.TempDir(), "triage.db")
	storeMu.Lock()
	storeInst = nil
	storeMu.Unlock()
	st, err := OpenStore(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rc := &runCtx{c: c, st: st, plan: &Plan{Number: 7, Close: ClosePromotional, ApproveRuns: []int64{1}}}

	if got := rc.next(ctx, "run-1"); !strings.Contains(got, "set_labels, close_pr, approve_ci, then finish_triage") {
		t.Fatalf("before any action: %q", got)
	}
	for _, a := range []string{ActionLabels, ActionClose} {
		if err := st.RecordAction(ctx, "run-1", a, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := rc.next(ctx, "run-1"); !strings.Contains(got, "Still required: approve_ci, then finish_triage") {
		t.Fatalf("after labels and close: %q", got)
	}
	if err := st.RecordAction(ctx, "run-1", ActionApprove, "", false); err != nil {
		t.Fatal(err)
	}
	if got := rc.next(ctx, "run-1"); !strings.Contains(got, "Call finish_triage now") {
		t.Fatalf("all done: %q", got)
	}
}
