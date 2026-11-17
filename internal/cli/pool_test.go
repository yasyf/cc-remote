package cli

import (
	"testing"
)

func TestPoolClaimChecksOutTheRequestedRefOnAWarmSpareBeforeItAnswers(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	standInFill(t)
	if _, err := openPool(w.session).fill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := w.session.Suspend(t.Context(), "pool-a"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	stale := gitOutput(t, w.root, "rev-parse", "HEAD")
	moved := moveOrigin(t, w.session.Config)
	claimed, err := claimWorkspace(t.Context(), w.session, "lane-a", "main")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Workspace != "pool-a" || claimed.Machine != "pool-a" || claimed.Allocation != allocationWarm {
		t.Errorf("claim = %+v, want warm pool-a", claimed)
	}
	if head := gitOutput(t, w.root, "rev-parse", "HEAD"); head != moved || head == stale {
		t.Errorf("checkout at %s, want the moved ref %s rather than the spare's %s", head, moved, stale)
	}
	if member := w.member("pool-a"); member.State != memberPrepared || member.Lane != "lane-a" || member.Failure != nil {
		t.Errorf("pool record = %+v, want pool-a prepared for lane-a", member)
	}
	w.free("pool-a")
}
