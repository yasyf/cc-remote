package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
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
	if _, err := stopLane(t.Context(), w.session, "lane-z"); err == nil || !strings.Contains(err.Error(), "no Orca task is recorded for lane-z") {
		t.Errorf("stop lane-z = %v, want no task recorded for it", err)
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
