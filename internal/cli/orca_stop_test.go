package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

func (w *firstWorker) laneWorkspace(name string) {
	w.t.Helper()
	if _, err := w.provider.Create(w.t.Context(), providers.Spec{Name: name, Labels: map[string]string{workspace.LabelWorkspace: name}}); err != nil {
		w.t.Fatal(err)
	}
	record := &workspace.Record{Name: name, Provider: "fake", Profile: "lean", Source: workspace.Source{Ref: "main"}, Machine: name}
	if err := state.Save(w.session.Config.State().Workspace(name), record); err != nil {
		w.t.Fatal(err)
	}
}

func (w *firstWorker) forwardedTask(name string) {
	w.t.Helper()
	dir := w.session.Config.State()
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: name, Provider: "fake", Profile: "lean", Machine: name, Environment: name, Forward: &orcaTunnel{Host: name, Config: "/recorded/config", Control: "/recorded/" + name + ".sock", Log: dir.OrcaForwardLog(name)}}
	if err := state.Save(dir.Orca(name), task); err != nil {
		w.t.Fatal(err)
	}
}

func stubForward(t *testing.T) string {
	t.Helper()
	bin, exits := t.TempDir(), filepath.Join(t.TempDir(), "exits")
	script := "#!/bin/sh\ncase \" $* \" in *' -O exit '*) echo \"$*\" >> " + exits + ";; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return exits
}

func TestStopClosesTheForwardAndSuspendsTheWorkspaceEachLaneRunsOn(t *testing.T) {
	w := newWarmPool(t)
	exits := stubForward(t)
	pool := openPool(w.session)
	w.laneWorkspace("lane-a")
	w.spare(pool, "pool-b")
	member := w.member("pool-b")
	member.State, member.Lane = memberPrepared, "lane-b"
	if err := pool.save(member); err != nil {
		t.Fatal(err)
	}
	w.forwardedTask("lane-a")
	w.forwardedTask("pool-b")

	for _, lane := range []string{"lane-a", "lane-b"} {
		stopped, err := stopLane(t.Context(), w.session, lane)
		if err != nil {
			t.Fatalf("stop %s = %v", lane, err)
		}
		if want := map[string]string{"lane-a": "lane-a", "lane-b": "pool-b"}[lane]; *stopped != (orcaStopped{Lane: lane, Workspace: want}) {
			t.Errorf("stop %s = %+v, want workspace %s", lane, stopped, want)
		}
	}

	for _, name := range []string{"lane-a", "pool-b"} {
		if !slices.Contains(w.provider.Calls(), "suspend "+name) {
			t.Errorf("%s was not suspended: %q", name, w.provider.Calls())
		}
		if machine, err := w.provider.Get(t.Context(), name); err != nil || machine.State != providers.StateSuspended {
			t.Errorf("%s after stop = %+v, %v; want it suspended", name, machine, err)
		}
		if _, err := loadTask(w.session.Config.State(), name); err != nil {
			t.Errorf("%s lost its task record: %v", name, err)
		}
		if _, err := w.session.Recorded(name); err != nil {
			t.Errorf("%s lost its workspace record: %v", name, err)
		}
	}
	closed, err := os.ReadFile(exits)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(closed)), "\n"); len(lines) != 2 || !strings.HasSuffix(lines[0], " lane-a") || !strings.HasSuffix(lines[1], " pool-b") {
		t.Errorf("forward exits = %q, want one for lane-a and one for pool-b", lines)
	}
	if member := w.member("pool-b"); member.State != memberPrepared || member.Lane != "lane-b" {
		t.Errorf("pool record after stop = %+v, want it kept as lane-b's", member)
	}
}

func TestStopNeverTouchesAWarmPoolWorkspaceNoLaneClaimed(t *testing.T) {
	w := newWarmPool(t)
	exits := stubForward(t)
	w.spare(openPool(w.session), "pool-c")
	before := len(w.provider.Calls())

	_, err := stopLane(t.Context(), w.session, "pool-c")
	if err == nil || !strings.Contains(err.Error(), "pool-c names a warm pool workspace, not a lane") {
		t.Errorf("stop pool-c = %v, want a refusal naming the warm pool workspace", err)
	}

	if calls := w.provider.Calls()[before:]; len(calls) != 0 {
		t.Errorf("a refused stop reached the provider: %q", calls)
	}
	if machine, err := w.provider.Get(t.Context(), "pool-c"); err != nil || machine.State != providers.StateRunning {
		t.Errorf("pool-c after the refusal = %+v, %v; want it running", machine, err)
	}
	if member := w.member("pool-c"); member.State != memberReady || member.Lane != "" {
		t.Errorf("pool-c's record after the refusal = %+v, want it ready and unclaimed", member)
	}
	if _, err := os.Stat(exits); !os.IsNotExist(err) {
		t.Errorf("a refused stop closed a forward: %v", err)
	}
}

type refusing struct {
	*providertest.Fake
}

func (r refusing) Suspend(context.Context, string) error {
	return errors.New("sprites api answered 500 stopping service claude")
}

func (w *firstWorker) recordsOf(name string) []string {
	w.t.Helper()
	dir := w.session.Config.State()
	var left []string
	for _, path := range []string{dir.Workspace(name), dir.Orca(name), dir.Pool(name)} {
		if _, err := os.Stat(path); err == nil {
			left = append(left, filepath.Base(filepath.Dir(path)))
		}
	}
	return left
}

func TestStopSuspendsALaneWorkspaceThatHasNoTaskRecord(t *testing.T) {
	w := newWarmPool(t)
	w.laneWorkspace("lane-d")

	stopped, err := stopLane(t.Context(), w.session, "lane-d")
	if err != nil {
		t.Fatalf("stop lane-d = %v", err)
	}
	if *stopped != (orcaStopped{Lane: "lane-d", Workspace: "lane-d"}) {
		t.Errorf("stop lane-d = %+v, want a plain stop", stopped)
	}
	if machine, err := w.provider.Get(t.Context(), "lane-d"); err != nil || machine.State != providers.StateSuspended {
		t.Errorf("lane-d after stop = %+v, %v; want it suspended", machine, err)
	}
	if _, err := w.session.Recorded("lane-d"); err != nil {
		t.Errorf("lane-d lost its workspace record: %v", err)
	}
}

func TestStopSucceedsAndCleansRecordsForALaneWithNothingLeftToStop(t *testing.T) {
	w := newWarmPool(t)
	exits := stubForward(t)
	pool := openPool(w.session)
	w.spare(pool, "pool-e")
	member := w.member("pool-e")
	member.State, member.Lane = memberPrepared, "lane-e"
	if err := pool.save(member); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(w.session.Config.State().Workspace("pool-e")); err != nil {
		t.Fatal(err)
	}
	w.laneWorkspace("lane-f")
	w.forwardedTask("lane-f")
	for _, name := range []string{"pool-e", "lane-f"} {
		if err := w.provider.Destroy(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	before := len(w.provider.Calls())

	for lane, cases := range map[string]struct{ workspace, found string }{
		"lane-e": {"pool-e", "no workspace is recorded for pool-e"},
		"lane-f": {"lane-f", "machine not found"},
		"lane-z": {"lane-z", "no workspace is recorded for lane-z"},
	} {
		stopped, err := stopLane(t.Context(), w.session, lane)
		if err != nil {
			t.Fatalf("stop %s = %v, want it already stopped", lane, err)
		}
		if stopped.Lane != lane || stopped.Workspace != cases.workspace || !strings.Contains(stopped.Found, cases.found) {
			t.Errorf("stop %s = %+v, want workspace %s found %q", lane, stopped, cases.workspace, cases.found)
		}
		if left := w.recordsOf(cases.workspace); len(left) != 0 {
			t.Errorf("stop %s left %q records for %s", lane, left, cases.workspace)
		}
	}

	if calls := w.provider.Calls()[before:]; !slices.Equal(calls, []string{"suspend lane-f"}) {
		t.Errorf("provider calls = %q, want only the suspend that found lane-f gone", calls)
	}
	closed, err := os.ReadFile(exits)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(closed)), " lane-f") {
		t.Errorf("forward exits = %q, %v; want lane-f's forward closed", closed, err)
	}
	if again, err := stopLane(t.Context(), w.session, "lane-f"); err != nil || again.Found == "" {
		t.Errorf("a second stop of lane-f = %+v, %v; want it already stopped", again, err)
	}
}

func TestStopFailsWhenTheMachineRefusesToSuspend(t *testing.T) {
	w := newWarmPool(t)
	w.laneWorkspace("lane-g")
	w.forwardedTask("lane-g")
	stubForward(t)
	session := w.over(refusing{w.provider})

	if _, err := stopLane(t.Context(), session, "lane-g"); err == nil || !strings.Contains(err.Error(), "answered 500") {
		t.Errorf("stop lane-g = %v, want the provider's refusal", err)
	}
	if left := w.recordsOf("lane-g"); !slices.Equal(left, []string{"workspaces", "orca"}) {
		t.Errorf("records for lane-g after the refusal = %q, want its workspace and task kept", left)
	}
	if machine, err := w.provider.Get(t.Context(), "lane-g"); err != nil || machine.State != providers.StateRunning {
		t.Errorf("lane-g after the refusal = %+v, %v; want it running", machine, err)
	}
}
