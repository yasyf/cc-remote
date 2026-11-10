package cli

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
)

func noPrune(context.Context) error { return nil }

func (w *firstWorker) pairedTask(name, provider, environmentID string, stopped bool) {
	w.t.Helper()
	dir := w.session.Config.State()
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: name, Provider: provider, Profile: "lean", Machine: name, Environment: name, EnvironmentID: environmentID, Stopped: stopped}
	if err := state.Save(dir.Orca(name), task); err != nil {
		w.t.Fatal(err)
	}
	if err := state.Write(dir.OrcaKept(name), nil); err != nil {
		w.t.Fatal(err)
	}
}

func TestPruneRemovesOnlyTheEnvironmentsOfStoppedLanesAndGoneMachines(t *testing.T) {
	w := newWarmPool(t)
	w.laneWorkspace("lane-live")
	w.laneWorkspace("lane-stopped")
	w.pairedTask("lane-live", "fake", "env-live", false)
	w.pairedTask("lane-stopped", "fake", "env-stopped", true)
	w.pairedTask("lane-gone", "fake", "env-gone", false)
	w.pairedTask("lane-repaired", "fake", "env-repaired-old", false)
	w.pairedTask("lane-elsewhere", "other", "env-elsewhere", false)
	saved := []orca.Environment{
		{ID: "env-live", Name: "lane-live"},
		{ID: "env-stopped", Name: "lane-stopped"},
		{ID: "env-gone", Name: "lane-gone"},
		{ID: "env-repaired-new", Name: "lane-repaired"},
		{ID: "env-elsewhere", Name: "lane-elsewhere"},
		{ID: "env-foreign", Name: "someone-else"},
	}
	want := []prunedEnvironment{
		{Workspace: "lane-stopped", Environment: "env-stopped", Reason: prunedStopped},
		{Workspace: "lane-gone", Environment: "env-gone", Reason: prunedGone},
	}

	dry := newSavedEnvironments(t, saved...)
	predicted, err := pruneEnvironments(t.Context(), w.session, dry.client(), true)
	if err != nil {
		t.Fatalf("prune --dry-run = %v", err)
	}
	if !predicted.DryRun || !slices.Equal(predicted.Forgotten, want) || len(dry.removed) != 0 {
		t.Errorf("prune --dry-run = %+v, removed %q; want %+v named and nothing removed", predicted, dry.removed, want)
	}

	environments := newSavedEnvironments(t, saved...)
	pruned, err := pruneEnvironments(t.Context(), w.session, environments.client(), false)
	if err != nil {
		t.Fatalf("prune = %v", err)
	}
	if pruned.DryRun || !slices.Equal(pruned.Forgotten, want) {
		t.Errorf("prune = %+v, want %+v", pruned, want)
	}
	if !slices.Equal(environments.removed, []string{"env-stopped", "env-gone"}) {
		t.Errorf("removed %q, want the stopped lane's and the gone machine's environments only", environments.removed)
	}
	dir := w.session.Config.State()
	for name, kept := range map[string]bool{"lane-live": true, "lane-stopped": true, "lane-gone": false, "lane-repaired": true, "lane-elsewhere": true} {
		if _, err := os.Stat(dir.OrcaKept(name)); (err == nil) != kept {
			t.Errorf("%s kept marker after prune: %v, want kept %t", name, err, kept)
		}
		if _, err := loadTask(dir, name); err != nil {
			t.Errorf("%s lost its task record: %v", name, err)
		}
	}

	again := newSavedEnvironments(t, environments.saved...)
	if rerun, err := pruneEnvironments(t.Context(), w.session, again.client(), false); err != nil || len(rerun.Forgotten) != 0 || len(again.removed) != 0 {
		t.Errorf("a second prune = %+v, %v, removed %q; want nothing left to remove", rerun, err, again.removed)
	}
}
