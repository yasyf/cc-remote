package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
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

func stubForwards(t *testing.T, hung, refused []string) func() []string {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	opens, up := filepath.Join(dir, "opens"), filepath.Join(dir, "up")
	script := "#!/bin/sh\nfor host do :; done\ncase \" $* \" in\n" +
		"*' -O check '*) grep -qx \"$host\" " + up + " 2>/dev/null; exit $?;;\n" +
		"*' -O exit '*) exit 0;;\n" +
		"*' -N -L '*)\n" +
		"  case ' " + strings.Join(hung, " ") + " ' in *\" $host \"*) exec sleep 60;; esac\n" +
		"  case ' " + strings.Join(refused, " ") + " ' in *\" $host \"*) exit 255;; esac\n" +
		"  echo \"$host\" >> " + opens + "; echo \"$host\" >> " + up + ";;\n" +
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

func storageReachable(context.Context, *orcaTask) error { return nil }

func storageGoneFor(machines ...string) storageCheck {
	return func(_ context.Context, task *orcaTask) error {
		if slices.Contains(machines, task.Machine) {
			return fmt.Errorf("sprite %s: %w", task.Machine, providers.ErrNoStorage)
		}
		return nil
	}
}

func TestKeepReopensAKeptForwardThatExitedButNotAStoppedOrUnkeptOne(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t, nil, nil)
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

	gone := map[string]time.Time{}
	kept, err := reopenForwards(t.Context(), dir, quiet, storageReachable, gone)
	if err != nil || kept != 1 {
		t.Fatalf("a keeper pass = %d kept, %v; want lane-a alone kept", kept, err)
	}
	if got := opened(); !slices.Equal(got, []string{"lane-a"}) {
		t.Errorf("forwards opened = %q, want only lane-a's, not the stopped lane-b's or the never-kept lane-c's", got)
	}
	if _, err := reopenForwards(t.Context(), dir, quiet, storageReachable, gone); err != nil || len(opened()) != 1 {
		t.Errorf("a pass over a forward that is up = %v, opened %q; want nothing reopened", err, opened())
	}
	if starts.Load() != 2 {
		t.Errorf("keeper starts = %d, want one per kept task", starts.Load())
	}
}

func TestKeepRunsOncePerStateDirectoryAndExitsWhenNothingIsKept(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t, nil, nil)
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
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable); err != nil || len(opened()) != 0 {
		t.Errorf("a keep beside a running one = %v, opened %q; want it to exit at once", err, opened())
	}
	unlock()

	if err := state.Remove(dir.OrcaKept("lane-a")); err != nil {
		t.Fatal(err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable); err != nil || len(opened()) != 0 {
		t.Errorf("a keep with nothing kept = %v, opened %q; want it to exit at once", err, opened())
	}
}

func (w *firstWorker) keptLanes(names ...string) {
	w.t.Helper()
	driver := orcaDriver{state: w.session.Config.State(), log: slog.New(slog.DiscardHandler)}
	for _, name := range names {
		w.laneWorkspace(name)
		if err := driver.keep(w.liveTask(name), w.session.Config.Path); err != nil {
			w.t.Fatal(err)
		}
	}
}

func TestKeepGivesEachReopenItsOwnDeadlineSoAHungForwardHoldsUpNoOther(t *testing.T) {
	within := reopenWithin
	reopenWithin = 200 * time.Millisecond
	t.Cleanup(func() { reopenWithin = within })
	w := newWarmPool(t)
	opened := stubForwards(t, []string{"lane-hung"}, nil)
	standInKeeper(t)
	w.keptLanes("lane-hung", "lane-next")
	var logged bytes.Buffer

	started := time.Now()
	kept, err := reopenForwards(t.Context(), w.session.Config.State(), slog.New(slog.NewTextHandler(&logged, nil)), storageReachable, map[string]time.Time{})
	if err != nil || kept != 2 {
		t.Fatalf("a pass = %d kept, %v; want both still kept", kept, err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the pass took %s; want the hung reopen cut at %s", took, reopenWithin)
	}
	if got := opened(); !slices.Equal(got, []string{"lane-next"}) {
		t.Errorf("forwards opened = %q, want lane-next's, behind the hung lane-hung", got)
	}
	if !strings.Contains(logged.String(), "workspace=lane-hung") || !strings.Contains(logged.String(), "did not finish within 200ms") {
		t.Errorf("the log %q does not name the hung reopen and its deadline", logged.String())
	}
}

func TestKeepReleasesAForwardWhoseMachineKeepsReportingItsStorageGoneAndSaysSoOnce(t *testing.T) {
	gone := storageGone
	storageGone = 0
	t.Cleanup(func() { storageGone = gone })
	w := newWarmPool(t)
	opened := stubForwards(t, nil, []string{"lane-dead", "lane-down"})
	standInKeeper(t)
	w.keptLanes("lane-dead", "lane-down", "lane-live")
	dir := w.session.Config.State()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	seen := map[string]time.Time{}
	storage := storageGoneFor("lane-dead")

	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 3 {
		t.Fatalf("the first pass = %d kept, %v; want all three kept, since one look at a gone storage releases nothing", kept, err)
	}
	if strings.Contains(logged.String(), "lane-dead") {
		t.Errorf("the first look at lane-dead's gone storage logged %q, want nothing until it is released", logged.String())
	}
	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the second pass = %d kept, %v; want lane-dead released", kept, err)
	}
	if _, err := os.Stat(dir.OrcaKept("lane-dead")); !os.IsNotExist(err) {
		t.Errorf("lane-dead is still kept: %v", err)
	}
	for _, still := range []string{"lane-down", "lane-live"} {
		if _, err := os.Stat(dir.OrcaKept(still)); err != nil {
			t.Errorf("%s was released, want it kept: %v", still, err)
		}
	}
	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the third pass = %d kept, %v", kept, err)
	}
	if n := strings.Count(logged.String(), "released a kept forward"); n != 1 || !strings.Contains(logged.String(), "workspace=lane-dead") {
		t.Errorf("logged %d releases in %q, want lane-dead's once", n, logged.String())
	}
	if n := strings.Count(logged.String(), "did not reopen, and the next pass tries again\" workspace=lane-down"); n != 3 {
		t.Errorf("lane-down, whose storage answers, logged %d failed reopens over three passes, want 3: %q", n, logged.String())
	}
	if got := opened(); !slices.Equal(got, []string{"lane-live"}) {
		t.Errorf("forwards opened = %q, want lane-live's alone", got)
	}
}

func TestKeepForgetsAGoneStorageSightingOnceItsLaneIsNoLongerKept(t *testing.T) {
	gone := storageGone
	storageGone = 0
	t.Cleanup(func() { storageGone = gone })
	w := newWarmPool(t)
	stubForwards(t, nil, []string{"lane-dead", "lane-other"})
	standInKeeper(t)
	w.keptLanes("lane-dead", "lane-other")
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	seen := map[string]time.Time{}
	storage := storageGoneFor("lane-dead")
	if _, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(dir.OrcaKept("lane-dead")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil {
		t.Fatal(err)
	}
	if err := state.Write(dir.OrcaKept("lane-dead"), nil); err != nil {
		t.Fatal(err)
	}
	if kept, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the first pass after lane-dead was kept again = %d kept, %v; want its sighting from before counted as new", kept, err)
	}
	if kept, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil || kept != 1 {
		t.Errorf("the second pass after it was kept again = %d kept, %v; want it released", kept, err)
	}
}
