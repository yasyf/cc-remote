package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
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

type savedEnvironments struct {
	t       *testing.T
	saved   []orca.Environment
	removed []string
	calls   int
}

func newSavedEnvironments(t *testing.T, saved ...orca.Environment) *savedEnvironments {
	return &savedEnvironments{t: t, saved: saved}
}

func (s *savedEnvironments) client() orca.Client { return orca.NewClient(s) }

func (s *savedEnvironments) Run(_ context.Context, args ...string) ([]byte, error) {
	s.calls++
	command := strings.Join(args, " ")
	if command == "environment list --json" {
		listed, err := json.Marshal(map[string][]orca.Environment{"environments": s.saved})
		if err != nil {
			return nil, err
		}
		return []byte(`{"id":"local","ok":true,"result":` + string(listed) + `,"_meta":{"runtimeId":"local"}}`), nil
	}
	id, ok := strings.CutPrefix(command, "environment rm --environment ")
	id, suffixed := strings.CutSuffix(id, " --json")
	if !ok || !suffixed {
		s.t.Errorf("unexpected orca call: %s", command)
		return nil, fmt.Errorf("unexpected orca call: %s", command)
	}
	s.saved = slices.DeleteFunc(s.saved, func(environment orca.Environment) bool { return environment.ID == id })
	s.removed = append(s.removed, id)
	return []byte(`{"id":"local","ok":true,"result":{"removed":{"id":"` + id + `"}},"_meta":{"runtimeId":"local"}}`), nil
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

func TestStopClosesTheForwardRemovesTheEnvironmentAndSuspendsTheWorkspaceEachLaneRunsOn(t *testing.T) {
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
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-a", Name: "lane-a"}, orca.Environment{ID: "env-b", Name: "pool-b"}, orca.Environment{ID: "env-c", Name: "pool-c"})

	for _, lane := range []string{"lane-a", "lane-b"} {
		stopped, err := stopLane(t.Context(), w.session, environments.client(), lane)
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
	if member := w.member("pool-b"); member.State != memberStopped || member.Lane != "lane-b" {
		t.Errorf("pool record after stop = %+v, want it kept as lane-b's, stopped", member)
	}
	if !slices.Equal(environments.removed, []string{"env-a", "env-b"}) || !slices.Equal(environments.saved, []orca.Environment{{ID: "env-c", Name: "pool-c"}}) {
		t.Errorf("Orca environments removed %q, left %+v; want lane-a's and pool-b's removed and pool-c's kept", environments.removed, environments.saved)
	}
	if again, err := stopLane(t.Context(), w.session, environments.client(), "lane-a"); err != nil || again.Found != "" {
		t.Errorf("a second stop of lane-a = %+v, %v; want it stopped again with nothing left to remove", again, err)
	}
	if len(environments.removed) != 2 {
		t.Errorf("a second stop removed %q, want nothing more", environments.removed[2:])
	}
}

func TestAStoppedLaneIsAdmittedAgainOnAFreshSpareAndRefusedWhileThatWorkerRuns(t *testing.T) {
	w := newWarmPool(t, "pool-b")
	standInFill(t)
	pool := openPool(w.session)
	w.claimedBy(pool, "pool-a", "lane-a", memberPrepared)
	if fill, err := pool.fill(t.Context()); err != nil || !slices.Equal(fill.Created, []string{"pool-b"}) {
		t.Fatalf("fill = %+v, %v", fill, err)
	}
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), w.contender(), runner, io.Discard, "lane-a", "")
	}, "lane lane-a already claimed warm workspace pool-a, now prepared; a lane gets one first worker until cc-remote orca stop lane-a stops it")

	if stopped, err := stopLane(t.Context(), w.session, newSavedEnvironments(t).client(), "lane-a"); err != nil || stopped.Workspace != "pool-a" {
		t.Fatalf("stop lane-a = %+v, %v; want pool-a stopped", stopped, err)
	}
	if workspace, err := pool.workspaceOf("lane-a"); err != nil || workspace != "lane-a" {
		t.Errorf("a stopped lane runs on %q, %v; want the lane's own name, so a workspace created under it is the one a later stop reaches", workspace, err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-b"))
	native, commands := w.nativeAs("pool-b")
	var out bytes.Buffer
	if err := w.warm(t.Context(), w.session, native, &out, "lane-a", ""); err != nil {
		t.Fatalf("relaunch = %v, want lane-a prepared on a fresh spare after its stop", err)
	}
	if allocation := w.allocation(out.Bytes()); allocation.Lane != "lane-a" || allocation.Workspace != "pool-b" || allocation.Task == nil || allocation.Task.Terminal != "term-1" {
		t.Errorf("allocation = %+v, want lane-a started on pool-b", allocation)
	}
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant one worker's\n%q", native.calls, commands)
	}
	for name, want := range map[string]memberState{"pool-a": memberStopped, "pool-b": memberPrepared} {
		if member := w.member(name); member.State != want || member.Lane != "lane-a" {
			t.Errorf("%s = %+v, want it %s for lane-a", name, member, want)
		}
	}
	w.free("pool-a")
	w.free("pool-b")
	w.free("lane-a")
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), w.contender(), runner, io.Discard, "lane-a", "")
	}, "lane lane-a already claimed warm workspace pool-b, now prepared")
	if workspace, err := pool.workspaceOf("lane-a"); err != nil || workspace != "pool-b" {
		t.Errorf("lane-a runs on %q, %v; want pool-b, not its stopped pool-a", workspace, err)
	}
}

func (w *firstWorker) claimedBy(pool *orcaPool, name, lane string, claim memberState) {
	w.t.Helper()
	w.spare(pool, name)
	member := w.member(name)
	member.State, member.Lane = claim, lane
	if err := pool.save(member); err != nil {
		w.t.Fatal(err)
	}
}

func TestStopSuspendsTheWorkspaceTheWorkerRunsOnNotAClaimThatFailedBeforeIt(t *testing.T) {
	w := newWarmPool(t)
	stubForward(t)
	pool := openPool(w.session)
	w.claimedBy(pool, "pool-a", "lane-moved", memberFailed)
	w.claimedBy(pool, "pool-b", "lane-moved", memberPrepared)
	w.claimedBy(pool, "pool-c", "lane-created", memberFailed)
	w.laneWorkspace("lane-created")
	w.claimedBy(pool, "pool-d", "lane-late", memberFailed)
	if err := state.Save(w.session.Config.State().Orca("pool-d"), &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "pool-d", Provider: "fake", Profile: "lean", Machine: "pool-d", Environment: "pool-d", Terminal: "term-d"}); err != nil {
		t.Fatal(err)
	}
	before := len(w.provider.Calls())
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-a", Name: "pool-a"}, orca.Environment{ID: "env-d", Name: "pool-d"})

	for lane, want := range map[string]string{"lane-moved": "pool-b", "lane-created": "lane-created", "lane-late": "pool-d"} {
		stopped, err := stopLane(t.Context(), w.session, environments.client(), lane)
		if err != nil {
			t.Fatalf("stop %s = %v", lane, err)
		}
		if stopped.Workspace != want {
			t.Errorf("stop %s reached %s, want %s", lane, stopped.Workspace, want)
		}
	}

	calls := w.provider.Calls()[before:]
	slices.Sort(calls)
	if want := []string{"suspend lane-created", "suspend pool-b", "suspend pool-d"}; !slices.Equal(calls, want) {
		t.Errorf("provider calls = %q, want %q", calls, want)
	}
	if !slices.Equal(environments.removed, []string{"env-d"}) {
		t.Errorf("Orca environments removed %q, want only pool-d's, not the failed claim pool-a's", environments.removed)
	}
}

func TestStopNeverTouchesAWarmPoolWorkspaceNoLaneClaimed(t *testing.T) {
	w := newWarmPool(t)
	exits := stubForward(t)
	w.spare(openPool(w.session), "pool-c")
	before := len(w.provider.Calls())
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-c", Name: "pool-c"})

	_, err := stopLane(t.Context(), w.session, environments.client(), "pool-c")
	if err == nil || !strings.Contains(err.Error(), "pool-c names a warm pool workspace, not a lane") {
		t.Errorf("stop pool-c = %v, want a refusal naming the warm pool workspace", err)
	}
	if environments.calls != 0 {
		t.Errorf("a refused stop reached Orca %d times", environments.calls)
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

type storageless struct {
	*providertest.Fake
}

func (storageless) Suspend(_ context.Context, id string) error {
	return fmt.Errorf("suspending sprite %s: %w: listing its services answered 503 Storage not ready", id, providers.ErrNoStorage)
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
	environments := newSavedEnvironments(t)

	stopped, err := stopLane(t.Context(), w.session, environments.client(), "lane-d")
	if err != nil {
		t.Fatalf("stop lane-d = %v", err)
	}
	if environments.calls != 0 {
		t.Errorf("a stop with no task record reached Orca %d times", environments.calls)
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
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-f", Name: "lane-f"})

	for lane, cases := range map[string]struct{ workspace, found string }{
		"lane-e": {"pool-e", "no workspace is recorded for pool-e"},
		"lane-f": {"lane-f", "machine not found"},
		"lane-z": {"lane-z", "no workspace is recorded for lane-z"},
	} {
		stopped, err := stopLane(t.Context(), w.session, environments.client(), lane)
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
	if !slices.Equal(environments.removed, []string{"env-f"}) {
		t.Errorf("Orca environments removed %q, want lane-f's", environments.removed)
	}
	if again, err := stopLane(t.Context(), w.session, environments.client(), "lane-f"); err != nil || again.Found == "" {
		t.Errorf("a second stop of lane-f = %+v, %v; want it already stopped", again, err)
	}
}

func TestStopFailsWhenTheMachineRefusesToSuspend(t *testing.T) {
	w := newWarmPool(t)
	w.laneWorkspace("lane-g")
	w.forwardedTask("lane-g")
	stubForward(t)
	session := w.over(refusing{w.provider})

	if _, err := stopLane(t.Context(), session, newSavedEnvironments(t).client(), "lane-g"); err == nil || !strings.Contains(err.Error(), "answered 500") {
		t.Errorf("stop lane-g = %v, want the provider's refusal", err)
	}
	if left := w.recordsOf("lane-g"); !slices.Equal(left, []string{"workspaces", "orca"}) {
		t.Errorf("records for lane-g after the refusal = %q, want its workspace and task kept", left)
	}
	if machine, err := w.provider.Get(t.Context(), "lane-g"); err != nil || machine.State != providers.StateRunning {
		t.Errorf("lane-g after the refusal = %+v, %v; want it running", machine, err)
	}
}

func TestStopOfAMachineWhoseStorageIsGoneSucceedsAndKeepsItsRecords(t *testing.T) {
	w := newWarmPool(t)
	w.laneWorkspace("lane-h")
	w.forwardedTask("lane-h")
	stubForward(t)
	session := w.over(storageless{w.provider})
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-h", Name: "lane-h"})

	stopped, err := stopLane(t.Context(), session, environments.client(), "lane-h")
	if err != nil {
		t.Fatalf("stop lane-h = %v, want it already stopped", err)
	}
	if !slices.Equal(environments.removed, []string{"env-h"}) {
		t.Errorf("Orca environments removed %q, want lane-h's, since its machine runs nothing again", environments.removed)
	}
	if stopped.Workspace != "lane-h" || !strings.Contains(stopped.Found, "Storage not ready") {
		t.Errorf("stop lane-h = %+v, want the missing storage named under found", stopped)
	}
	if left := w.recordsOf("lane-h"); !slices.Equal(left, []string{"workspaces", "orca"}) {
		t.Errorf("records for lane-h after the stop = %q, want its workspace and task kept", left)
	}
	if _, err := w.provider.Get(t.Context(), "lane-h"); err != nil {
		t.Errorf("lane-h after the stop = %v, want the machine left in place", err)
	}
}

func TestDestroyRemovesTheOrcaEnvironmentOfAWorkspaceWithATask(t *testing.T) {
	w := newWarmPool(t)
	exits := stubForward(t)
	w.laneWorkspace("lane-x")
	w.forwardedTask("lane-x")
	w.laneWorkspace("lane-y")
	environments := newSavedEnvironments(t, orca.Environment{ID: "env-x", Name: "lane-x"}, orca.Environment{ID: "env-z", Name: "lane-z"})

	if err := destroyWorkspace(t.Context(), w.session, environments.client(), "lane-x"); err != nil {
		t.Fatalf("destroy lane-x = %v", err)
	}
	if _, err := w.provider.Get(t.Context(), "lane-x"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("lane-x after destroy = %v, want it gone", err)
	}
	if !slices.Equal(environments.removed, []string{"env-x"}) || !slices.Equal(environments.saved, []orca.Environment{{ID: "env-z", Name: "lane-z"}}) {
		t.Errorf("Orca environments removed %q, left %+v; want lane-x's removed and lane-z's kept", environments.removed, environments.saved)
	}
	if closed, err := os.ReadFile(exits); err != nil || !strings.HasSuffix(strings.TrimSpace(string(closed)), " lane-x") {
		t.Errorf("forward exits = %q, %v; want lane-x's forward closed", closed, err)
	}

	calls := environments.calls
	if err := destroyWorkspace(t.Context(), w.session, environments.client(), "lane-y"); err != nil {
		t.Fatalf("destroy lane-y = %v", err)
	}
	if environments.calls != calls {
		t.Errorf("destroying lane-y, which has no task record, reached Orca %d times", environments.calls-calls)
	}
}
