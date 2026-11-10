package cli

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yasyf/cc-remote/internal/state"
)

func standInKeeper(t *testing.T) *atomic.Int32 {
	t.Helper()
	starts := &atomic.Int32{}
	original := keepCommand
	keepCommand = func(string) (*exec.Cmd, error) {
		starts.Add(1)
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { keepCommand = original })
	return starts
}

func stubForwards(t *testing.T) func() []string {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	opens, up := filepath.Join(dir, "opens"), filepath.Join(dir, "up")
	script := "#!/bin/sh\nfor host do :; done\ncase \" $* \" in\n" +
		"*' -O check '*) grep -qx \"$host\" " + up + " 2>/dev/null; exit $?;;\n" +
		"*' -O exit '*) exit 0;;\n" +
		"*' -N -L '*) echo \"$host\" >> " + opens + "; echo \"$host\" >> " + up + ";;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return func() []string {
		raw, err := os.ReadFile(opens)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(raw))
	}
}

func (w *firstWorker) liveTask(name string) *orcaTask {
	w.t.Helper()
	control, err := state.NewOrcaControl()
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(control)) })
	dir := w.session.Config.State()
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: name, Provider: "fake", Profile: "lean", Machine: name, Environment: name, Port: 7001, Forward: &orcaTunnel{Host: name, Config: "/recorded/config", Control: control, Log: dir.OrcaForwardLog(name)}}
	if err := state.Save(dir.Orca(name), task); err != nil {
		w.t.Fatal(err)
	}
	return task
}

func TestKeepReopensAKeptForwardThatExitedButNotAStoppedOrUnkeptOne(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t)
	starts := standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	driver := orcaDriver{state: dir, log: quiet}
	for _, name := range []string{"lane-a", "lane-b", "lane-c"} {
		w.laneWorkspace(name)
		task := w.liveTask(name)
		if name == "lane-c" {
			continue
		}
		if err := driver.keep(task, w.session.Config.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stopLane(t.Context(), w.session, newSavedEnvironments(t).client(), "lane-b"); err != nil {
		t.Fatalf("stop lane-b = %v", err)
	}

	kept, err := reopenForwards(t.Context(), dir, quiet)
	if err != nil || kept != 1 {
		t.Fatalf("a keeper pass = %d kept, %v; want lane-a alone kept", kept, err)
	}
	if got := opened(); !slices.Equal(got, []string{"lane-a"}) {
		t.Errorf("forwards opened = %q, want only lane-a's, not the stopped lane-b's or the never-kept lane-c's", got)
	}
	if _, err := reopenForwards(t.Context(), dir, quiet); err != nil || len(opened()) != 1 {
		t.Errorf("a pass over a forward that is up = %v, opened %q; want nothing reopened", err, opened())
	}
	if starts.Load() != 2 {
		t.Errorf("keeper starts = %d, want one per kept task", starts.Load())
	}
}

func TestKeepRunsOncePerStateDirectoryAndExitsWhenNothingIsKept(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t)
	standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	w.laneWorkspace("lane-a")
	if err := (orcaDriver{state: dir, log: quiet}).keep(w.liveTask("lane-a"), w.session.Config.Path); err != nil {
		t.Fatal(err)
	}

	unlock, held, err := state.TryLock(dir.OrcaKeeper())
	if err != nil || !held {
		t.Fatalf("hold the keeper lock = %t, %v", held, err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune); err != nil || len(opened()) != 0 {
		t.Errorf("a keep beside a running one = %v, opened %q; want it to exit at once", err, opened())
	}
	unlock()

	if err := state.Remove(dir.OrcaKept("lane-a")); err != nil {
		t.Fatal(err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune); err != nil || len(opened()) != 0 {
		t.Errorf("a keep with nothing kept = %v, opened %q; want it to exit at once", err, opened())
	}
}
