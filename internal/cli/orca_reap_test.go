package cli

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
)

func (w *firstWorker) settleMember(pool *orcaPool, name, lane string, to memberState) {
	w.t.Helper()
	member := w.member(name)
	member.State, member.Lane = to, lane
	if err := pool.save(member); err != nil {
		w.t.Fatal(err)
	}
}

func TestReapDestroysReleasedLanesAndForgetsWorkspacesWhoseMachineIsGone(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	dir := w.session.Config.State()
	w.spare(pool, "pool-released")
	w.spare(pool, "pool-prepared")
	w.spare(pool, "pool-gone")
	w.settleMember(pool, "pool-released", "lane-a", memberReleased)
	w.settleMember(pool, "pool-prepared", "lane-b", memberPrepared)
	w.settleMember(pool, "pool-gone", "lane-c", memberFailed)
	if err := w.provider.Destroy(t.Context(), "pool-gone"); err != nil {
		t.Fatal(err)
	}
	w.pairedTask("pool-released", "fake", "env-released", false)
	w.pairedTask("pool-prepared", "fake", "env-prepared", false)
	w.pairedTask("pool-gone", "fake", "env-gone", false)
	w.pairedTask("lane-old", "fake", "", false)
	w.pairedTask("lane-elsewhere", "other", "", false)
	if err := state.Write(dir.Workspace("pool-dead")+".lock", nil); err != nil {
		t.Fatal(err)
	}
	environments := newSavedEnvironments(t,
		orca.Environment{ID: "env-released", Name: "pool-released"},
		orca.Environment{ID: "env-prepared", Name: "pool-prepared"},
		orca.Environment{ID: "env-gone", Name: "pool-gone"},
	)

	reaped, err := pool.reap(t.Context(), environments.client())
	if err != nil {
		t.Fatalf("reap = %v", err)
	}
	if !slices.Equal(reaped.Destroyed, []string{"pool-released"}) || !slices.Equal(reaped.Forgotten, []string{"pool-gone", "lane-old"}) || reaped.Locks != 1 {
		t.Errorf("reaped = %+v, want pool-released destroyed, pool-gone and lane-old forgotten, one lock removed", reaped)
	}
	if !slices.Equal(environments.removed, []string{"env-released", "env-gone"}) {
		t.Errorf("removed environments %q, want the released and the gone workspaces' only", environments.removed)
	}
	if _, err := w.provider.Get(t.Context(), "pool-released"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("released machine = %v, want destroyed", err)
	}
	if _, err := w.provider.Get(t.Context(), "pool-prepared"); err != nil {
		t.Errorf("prepared machine = %v, want kept", err)
	}
	for _, name := range []string{"pool-released", "pool-gone", "lane-old"} {
		for _, path := range []string{dir.Pool(name), dir.Workspace(name), dir.Orca(name), dir.Orca(name) + ".lock", dir.OrcaKept(name)} {
			if recorded(path) {
				t.Errorf("%s stays after the reap", path)
			}
		}
	}
	for _, path := range []string{dir.Pool("pool-prepared"), dir.Workspace("pool-prepared"), dir.Orca("pool-prepared"), dir.OrcaKept("pool-prepared"), dir.Orca("lane-elsewhere")} {
		if !recorded(path) {
			t.Errorf("%s was removed, want it kept", path)
		}
	}
	if recorded(dir.Workspace("pool-dead") + ".lock") {
		t.Error("the orphaned workspace lock stays")
	}
}

func TestReapLeavesAWorkspaceALaunchHolds(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	w.spare(pool, "pool-held")
	w.settleMember(pool, "pool-held", "lane-a", memberReleased)
	unlock, err := state.Lock(w.session.Config.State().Orca("pool-held") + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	reaped, err := pool.reap(t.Context(), newSavedEnvironments(t).client())
	if err != nil || len(reaped.Destroyed) != 0 {
		t.Fatalf("reap = %+v, %v, want nothing destroyed", reaped, err)
	}
	if _, err := w.provider.Get(t.Context(), "pool-held"); err != nil {
		t.Errorf("held machine = %v, want kept", err)
	}
	if got := w.member("pool-held").State; got != memberReleased {
		t.Errorf("state = %s, want it still released for the next fill", got)
	}
}

func TestReleaseDestroysEveryWorkspaceTheLaneClaimed(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	dir := w.session.Config.State()
	w.spare(pool, "pool-a")
	w.spare(pool, "pool-b")
	w.spare(pool, "pool-other")
	w.settleMember(pool, "pool-a", "lane-a", memberPrepared)
	w.settleMember(pool, "pool-b", "lane-a", memberStopped)
	w.settleMember(pool, "pool-other", "lane-b", memberPrepared)
	w.pairedTask("pool-a", "fake", "env-a", false)
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-a", Name: "pool-a"})

	released, err := releaseLane(t.Context(), w.session, environments.client(), "lane-a")
	if err != nil {
		t.Fatalf("release = %v", err)
	}
	if released.Lane != "lane-a" || !slices.Equal(released.Workspaces, []string{"pool-a", "pool-b"}) {
		t.Errorf("released = %+v, want pool-a and pool-b", released)
	}
	for _, name := range []string{"pool-a", "pool-b"} {
		if _, err := w.provider.Get(t.Context(), name); !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("%s = %v, want destroyed", name, err)
		}
		if recorded(dir.Pool(name)) || recorded(dir.Workspace(name)) || recorded(dir.Orca(name)) {
			t.Errorf("records of %s stay after release", name)
		}
	}
	if !slices.Equal(environments.removed, []string{"env-a"}) {
		t.Errorf("removed environments %q, want env-a", environments.removed)
	}
	if got := w.member("pool-other").State; got != memberPrepared {
		t.Errorf("another lane's workspace = %s, want untouched", got)
	}
	if _, err := releaseLane(t.Context(), w.session, environments.client(), "pool-other"); err == nil {
		t.Error("release of a pool workspace's own name succeeded, want it refused")
	}
}

type listMisses struct {
	providers.Provider
	missing string
}

func (l listMisses) List(ctx context.Context, labels map[string]string) ([]providers.Machine, error) {
	machines, err := l.Provider.List(ctx, labels)
	return slices.DeleteFunc(machines, func(machine providers.Machine) bool { return machine.ID == l.missing }), err
}

func TestReapKeepsAWorkspaceTheListMissedButTheProviderStillHas(t *testing.T) {
	w := newWarmPool(t)
	w.spare(openPool(w.session), "pool-hidden")
	w.settleMember(openPool(w.session), "pool-hidden", "lane-a", memberPrepared)
	w.pairedTask("pool-hidden", "fake", "env-hidden", false)
	pool := openPool(w.over(listMisses{Provider: w.provider, missing: "pool-hidden"}))
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-hidden", Name: "pool-hidden"})

	reaped, err := pool.reap(t.Context(), environments.client())
	if err != nil || len(reaped.Forgotten) != 0 || len(reaped.Destroyed) != 0 {
		t.Fatalf("reap = %+v, %v, want nothing reaped", reaped, err)
	}
	if _, err := w.provider.Get(t.Context(), "pool-hidden"); err != nil {
		t.Errorf("machine = %v, want kept", err)
	}
	dir := w.session.Config.State()
	for _, path := range []string{dir.Pool("pool-hidden"), dir.Workspace("pool-hidden"), dir.Orca("pool-hidden"), dir.OrcaKept("pool-hidden")} {
		if !recorded(path) {
			t.Errorf("%s was removed, want it kept", path)
		}
	}
	if len(environments.removed) != 0 {
		t.Errorf("removed environments %q, want none", environments.removed)
	}
}
