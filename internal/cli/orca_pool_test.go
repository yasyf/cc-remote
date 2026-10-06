package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
	"github.com/yasyf/cc-remote/internal/workspace/workspacetest"
)

const spriteFails = "echo 'fatal: sk-leakedsecret answered by the synthetic provider' >&2\nexit 1\n"

type observing struct {
	*providertest.Fake
	paused *gate
	on     string
	err    error
}

type rejecting struct {
	*providertest.Fake
	name    string
	creates atomic.Int32
}

func (o *observing) Get(ctx context.Context, id string) (providers.Machine, error) {
	if id == o.on && o.paused != nil {
		o.paused.hold()
	}
	if id == o.on && o.err != nil {
		return providers.Machine{}, o.err
	}
	return o.Fake.Get(ctx, id)
}

func (r *rejecting) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	if spec.Name != r.name {
		return r.Fake.Create(ctx, spec)
	}
	r.creates.Add(1)
	rejected := &providers.CommandError{Command: "sprite create -o test --skip-console " + spec.Name, Result: providers.Result{ExitCode: 1, Stderr: []byte("fatal: sk-leakedsecret rejected https://api.sprites.dev/v1/sprites?token=sk-leakedsecret")}}
	return providers.Machine{ID: spec.Name, Provider: "fake", State: providers.StateUnknown}, &sprites.CreateError{Step: sprites.StepCommand, Err: fmt.Errorf("sprite %s: whether the create allocated it is unknown: %w", spec.Name, rejected)}
}

func newWarmPool(t *testing.T, names ...string) *firstWorker {
	t.Helper()
	w := newFirstWorker(t, nil, false)
	lean := w.session.Config.Profiles["lean"]
	lean.Prepare = nil
	w.session.Config.Profiles["lean"] = lean
	w.session = w.over(w.provider)
	w.session.Token = func(context.Context) (string, error) { return workspacetest.Token, nil }
	w.session.Stderr = io.Discard
	if err := w.session.Poolable(); err != nil {
		t.Fatal(err)
	}
	nameSpares(t, names...)
	return w
}

func nameSpares(t *testing.T, names ...string) {
	t.Helper()
	next := &atomic.Int32{}
	original := spareName
	spareName = func() string {
		taken := int(next.Add(1))
		if taken > len(names) {
			t.Errorf("a fill made more spares than %q", names)
			return fmt.Sprintf("pool-extra-%d", taken)
		}
		return names[taken-1]
	}
	t.Cleanup(func() { spareName = original })
}

func standInFill(t *testing.T) *atomic.Int32 {
	t.Helper()
	starts := &atomic.Int32{}
	original := fillCommand
	fillCommand = func(string, string, string) (*exec.Cmd, error) {
		starts.Add(1)
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { fillCommand = original })
	return starts
}

func moveOrigin(t *testing.T, cfg *config.Config) string {
	t.Helper()
	origin := strings.TrimPrefix(cfg.Repository, "file://")
	gitOutput(t, origin, "commit", "-q", "--allow-empty", "-m", "moved")
	return gitOutput(t, origin, "rev-parse", "HEAD")
}

func (w *firstWorker) over(provider providers.Provider) *workspace.Session {
	w.t.Helper()
	session, err := workspace.Open(w.session.Config, provider, "fake", "lean", w.platform)
	if err != nil {
		w.t.Fatal(err)
	}
	session.Log = slog.New(slog.DiscardHandler)
	return session
}

func (w *firstWorker) warm(ctx context.Context, session *workspace.Session, runner orca.Runner, out io.Writer, lane, ref string) error {
	launch := orcaLaunch{agent: firstAgent, warm: true, ref: ref}
	return launch.allocate(ctx, out, session, runner, orca.Runtime{Entry: "tools/orca/AppRun"}, lane, func(ctx context.Context, driver orcaDriver, task *orcaTask) error {
		return driver.publish(ctx, task, []byte("Read every line.\n"))
	})
}

func (w *firstWorker) spare(pool *orcaPool, name string) {
	w.t.Helper()
	if _, err := w.provider.Create(w.t.Context(), providers.Spec{Name: name, Labels: map[string]string{workspace.LabelWorkspace: name, workspace.LabelPool: pool.key}}); err != nil {
		w.t.Fatal(err)
	}
	record := &workspace.Record{Name: name, Provider: "fake", Profile: "lean", Source: workspace.Source{Ref: "main"}, Machine: name}
	if err := state.Save(w.session.Config.State().Workspace(name), record); err != nil {
		w.t.Fatal(err)
	}
	member := &orcaMember{SchemaVersion: orcaPoolSchema, Name: name, Provider: "fake", Profile: "lean", Key: pool.key, State: memberReady, CreatedAt: time.Now().UTC()}
	if err := pool.save(member); err != nil {
		w.t.Fatal(err)
	}
}

func (w *firstWorker) member(name string) *orcaMember {
	w.t.Helper()
	member, err := openPool(w.session).load(name)
	if err != nil || member == nil {
		w.t.Fatalf("pool record %s = %+v, %v", name, member, err)
	}
	return member
}

func (w *firstWorker) allocation(out []byte) orcaAllocation {
	w.t.Helper()
	var allocation orcaAllocation
	if err := json.Unmarshal(out, &allocation); err != nil {
		w.t.Fatalf("allocation %s: %v", out, err)
	}
	if allocation.Task != nil && allocation.Task.Forward != nil {
		w.t.Cleanup(func() { _ = os.Remove(filepath.Dir(allocation.Task.Forward.Control)) })
	}
	if bytes.Contains(out, []byte("sk-test-key")) {
		w.t.Errorf("the key reached the allocation: %s", out)
	}
	return allocation
}

func TestWarmPrepareClaimsAnUnusedSpareRefreshedToTheMovedRef(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	fills := standInFill(t)
	if fill, err := openPool(w.session).fill(t.Context()); err != nil || !slices.Equal(fill.Created, []string{"pool-a"}) || !slices.Equal(fill.Ready, []string{"pool-a"}) {
		t.Fatalf("fill = %+v, %v", fill, err)
	}
	if err := w.session.Suspend(t.Context(), "pool-a"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	stale := gitOutput(t, w.root, "rev-parse", "HEAD")
	gitOutput(t, w.root, "config", "cc-remote.kept", "existing")
	moved := moveOrigin(t, w.session.Config)
	calls, scripts := len(w.provider.Calls()), len(w.local.Scripts("pool-a"))
	native, commands := w.nativeAs("pool-a")
	var out bytes.Buffer
	if err := w.warm(t.Context(), w.session, native, &out, "lane-a", ""); err != nil {
		t.Fatal(err)
	}
	allocation := w.allocation(out.Bytes())
	if allocation.SchemaVersion != 1 || allocation.Lane != "lane-a" || allocation.Workspace != "pool-a" || allocation.Allocation != allocationWarm || allocation.Ref != "main" || allocation.Head != moved || stale == moved {
		t.Errorf("allocation = %+v, want lane-a on pool-a at %s rather than %s", allocation, moved, stale)
	}
	home, err := filepath.EvalSymlinks(w.local.Home("pool-a"))
	if err != nil {
		t.Fatal(err)
	}
	brief := artifactOf(home+"/.cc-remote/orca/tasks/pool-a/brief.md", []byte("Read every line.\n"))
	task := allocation.Task
	if task == nil || task.Workspace != "pool-a" || task.Environment != "pool-a" || task.Machine != "pool-a" || task.BaseCommit != moved || !task.Prepared || task.Terminal != "term-1" || task.Brief == nil || *task.Brief != brief {
		t.Errorf("task = %+v, want pool-a prepared at %s with %+v", task, moved, brief)
	}
	if loaded, err := loadTask(w.session.Config.State(), "pool-a"); err != nil || loaded.BaseCommit != moved || !loaded.Prepared {
		t.Errorf("stored task = %+v, %v", loaded, err)
	}
	if found, err := state.Load(w.session.Config.State().Orca("lane-a"), &orcaTask{}); err != nil || found {
		t.Errorf("a task was recorded under the lane label: %t, %v", found, err)
	}
	if replenish := allocation.Replenish; replenish == nil || replenish.Target != 1 || replenish.PID == 0 || replenish.Failure != nil || fills.Load() != 1 {
		t.Errorf("replenish = %+v after %d fills, want one started", replenish, fills.Load())
	}
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant\n%q", native.calls, commands)
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "HEAD"}, moved},
		{[]string{"rev-parse", "--is-shallow-repository"}, "true"},
		{[]string{"rev-list", "--count", "HEAD"}, "1"},
		{[]string{"status", "--porcelain", "--untracked-files=all"}, ""},
		{[]string{"config", "--get", "cc-remote.kept"}, "existing"},
	} {
		if got := gitOutput(t, w.root, check.args...); got != check.want {
			t.Errorf("git %v = %q, want %q", check.args, got, check.want)
		}
	}
	lifecycle := w.provider.Calls()[calls:]
	if !slices.Contains(lifecycle, "wake pool-a") || slices.ContainsFunc(lifecycle, func(call string) bool {
		return strings.HasPrefix(call, "create ") || strings.HasPrefix(call, "destroy ")
	}) {
		t.Errorf("provider calls = %q, want pool-a woken and nothing created or destroyed", lifecycle)
	}
	refreshed := w.local.Scripts("pool-a")[scripts:]
	if !slices.ContainsFunc(refreshed, func(script string) bool { return strings.Contains(script, "fetch --quiet --prune --depth 1 origin") }) {
		t.Errorf("no shallow fetch refreshed the checkout: %q", refreshed)
	}
	for _, script := range refreshed {
		if strings.Contains(script, "plugins.sh install") || strings.Contains(script, "plugins.sh publish") {
			t.Errorf("the claimed spare was provisioned again: %q", script)
		}
	}
	if keys, err := os.ReadFile(w.keys); err != nil || string(keys) != "read\n" {
		t.Errorf("key reads = %q, %v; want one", keys, err)
	}
	if member := w.member("pool-a"); member.State != memberPrepared || member.Lane != "lane-a" || member.Ref != "main" || member.Head != moved || member.Failure != nil || member.Reason != "" {
		t.Errorf("pool record = %+v", member)
	}
	stored, err := os.ReadFile(w.session.Config.State().Orca("pool-a"))
	if err != nil || bytes.Contains(stored, []byte("sk-test-key")) {
		t.Errorf("the key reached the task record: %s, %v", stored, err)
	}
	w.free("pool-a")
	w.free("lane-a")
}

func TestWarmPrepareCreatesTheLaneWorkspaceWhenThePoolIsEmpty(t *testing.T) {
	w := newWarmPool(t)
	fills := standInFill(t)
	t.Setenv("REMOTE_HOME", w.local.Home("lane-a"))
	native, commands := w.nativeAs("lane-a")
	var out bytes.Buffer
	if err := w.warm(t.Context(), w.session, native, &out, "lane-a", "feature"); err != nil {
		t.Fatal(err)
	}
	allocation := w.allocation(out.Bytes())
	head := gitOutput(t, w.root, "rev-parse", "HEAD")
	if allocation.Allocation != allocationCreated || allocation.Lane != "lane-a" || allocation.Workspace != "lane-a" || allocation.Ref != "feature" || allocation.Head != head {
		t.Errorf("allocation = %+v, want lane-a created at %s", allocation, head)
	}
	if task := allocation.Task; task == nil || task.Workspace != "lane-a" || !task.Prepared || task.BaseCommit != head {
		t.Errorf("task = %+v", task)
	}
	if branch := gitOutput(t, w.root, "rev-parse", "--abbrev-ref", "HEAD"); branch != "feature" {
		t.Errorf("checked out %q, want feature", branch)
	}
	creates := 0
	for _, call := range w.provider.Calls() {
		switch {
		case call == "create lane-a":
			creates++
		case strings.HasPrefix(call, "create "), strings.HasPrefix(call, "wake "):
			t.Errorf("an empty pool made %q", call)
		}
	}
	if creates != 1 || !slices.Equal(native.calls, commands) {
		t.Errorf("creates = %d, orca calls =\n%q\nwant one create and\n%q", creates, native.calls, commands)
	}
	if replenish := allocation.Replenish; replenish == nil || replenish.PID == 0 || fills.Load() != 1 {
		t.Errorf("replenish = %+v after %d fills, want one started for the empty pool", replenish, fills.Load())
	}
	if members, err := openPool(w.session).members(); err != nil || len(members) != 0 {
		t.Errorf("pool records = %+v, %v; the lane's own workspace is not a pool member", members, err)
	}
	w.free("lane-a")
	later := w.contender()
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), later, runner, io.Discard, "lane-a", "")
	}, "lane-a already has an Orca task")
	if fills.Load() != 1 {
		t.Errorf("a refused request started a fill: %d", fills.Load())
	}
}

func TestSimultaneousWarmClaimsTakeDistinctSpares(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	w.spare(pool, "pool-a")
	w.spare(pool, "pool-b")
	observed := &observing{Fake: w.provider, paused: newGate(), on: "pool-a"}
	calls := len(w.provider.Calls())
	type claimed struct {
		member  *orcaMember
		release func()
		err     error
	}
	done := make(chan claimed, 1)
	first := openPool(w.over(observed))
	go func() {
		member, release, err := first.claim(t.Context(), "lane-a", "main")
		done <- claimed{member, release, err}
	}()
	select {
	case <-observed.paused.reached:
	case got := <-done:
		t.Fatalf("the first claim returned %+v, %v before it observed pool-a", got.member, got.err)
	}
	second, release, err := openPool(w.contender()).claim(t.Context(), "lane-b", "main")
	if err != nil || second == nil || second.Name != "pool-b" || second.Lane != "lane-b" || second.State != memberClaimed {
		t.Fatalf("second claim = %+v, %v; want pool-b while pool-a is under observation", second, err)
	}
	release()
	if third, _, err := openPool(w.contender()).claim(t.Context(), "lane-c", "main"); err != nil || third != nil {
		t.Errorf("third claim = %+v, %v; want none while pool-a is held and pool-b is claimed", third, err)
	}
	close(observed.paused.release)
	got := <-done
	if got.err != nil || got.member == nil || got.member.Name != "pool-a" || got.member.Lane != "lane-a" {
		t.Fatalf("first claim = %+v, %v; want pool-a", got.member, got.err)
	}
	got.release()
	for name, lane := range map[string]string{"pool-a": "lane-a", "pool-b": "lane-b"} {
		if member := w.member(name); member.State != memberClaimed || member.Lane != lane || member.Ref != "main" {
			t.Errorf("%s = %+v, want it claimed by %s", name, member, lane)
		}
	}
	if after := w.provider.Calls()[calls:]; len(after) != 0 {
		t.Errorf("the claims touched the spares: %q", after)
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("a claim read the key: %v", err)
	}
	if again, _, err := openPool(w.contender()).claim(t.Context(), "lane-d", "main"); err != nil || again != nil {
		t.Errorf("a claim after both were taken = %+v, %v", again, err)
	}
	w.free("pool-a")
	w.free("pool-b")
}

func TestCompetingRequestsAreRefusedBeforeTheClaimedSpareIsTouched(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	standInFill(t)
	if _, err := openPool(w.session).fill(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	native, commands := w.nativeAs("pool-a")
	checking := newGate()
	w.paused = checking
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- w.warm(t.Context(), w.session, native, &out, "lane-a", "") }()
	w.reach(checking, done)
	if member := w.member("pool-a"); member.State != memberClaimed || member.Lane != "lane-a" {
		t.Fatalf("pool-a = %+v before its first check, want it claimed", member)
	}
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), w.contender(), runner, io.Discard, "lane-a", "")
	}, "another cc-remote command holds the Orca task for lane-a")
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.prepare(t.Context(), w.contender(), runner, io.Discard, "pool-a", true)
	}, "another cc-remote command holds the Orca task for pool-a")
	before := w.effects()
	if member, _, err := openPool(w.contender()).claim(t.Context(), "lane-b", "main"); err != nil || member != nil {
		t.Errorf("a claim for another lane = %+v, %v; want none", member, err)
	}
	if after := w.effects(); after != before {
		t.Errorf("a competing claim changed %+v into %+v", before, after)
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("the key was read before the claimed spare passed its checks: %v", err)
	}
	close(checking.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if allocation := w.allocation(out.Bytes()); allocation.Workspace != "pool-a" || allocation.Allocation != allocationWarm {
		t.Errorf("allocation = %+v", allocation)
	}
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant one worker's\n%q", native.calls, commands)
	}
	if keys, err := os.ReadFile(w.keys); err != nil || string(keys) != "read\n" {
		t.Errorf("key reads = %q, %v; want only the claimant's", keys, err)
	}
	w.free("pool-a")
	w.free("lane-a")
}

func TestAFailedClaimStaysClaimedAndNoOtherSpareIsTried(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	fills := standInFill(t)
	pool := openPool(w.session)
	if _, err := pool.fill(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.spare(pool, "pool-b")
	if err := os.WriteFile(filepath.Join(w.root, "notes.txt"), []byte("left behind\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	calls := len(w.provider.Calls())
	native := 0
	runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
		native++
		return nil, errors.New("a failed claim reached the Orca CLI")
	})
	var out bytes.Buffer
	err := w.warm(t.Context(), w.session, runner, &out, "lane-a", "")
	if err == nil || !strings.Contains(err.Error(), "lane lane-a claimed warm workspace pool-a") || !strings.Contains(err.Error(), "check the unused checkout on pool-a exited 8: the checkout has uncommitted or untracked changes") {
		t.Fatalf("warm = %v", err)
	}
	if out.Len() != 0 || native != 0 {
		t.Errorf("a failed claim printed %q after %d Orca calls", out.String(), native)
	}
	if member := w.member("pool-a"); member.State != memberFailed || member.Lane != "lane-a" || member.Failure == nil || member.Failure.Bytes == 0 || len(member.Failure.SHA256) != 64 || member.Failure.HTTPStatus != nil {
		t.Errorf("pool-a = %+v, want it failed for lane-a with error metadata", member)
	}
	if stored, err := os.ReadFile(w.session.Config.State().Pool("pool-a")); err != nil || bytes.Contains(stored, []byte("uncommitted")) || bytes.Contains(stored, []byte("exited")) {
		t.Errorf("the pool record kept error text: %s, %v", stored, err)
	}
	if member := w.member("pool-b"); member.State != memberReady || member.Lane != "" {
		t.Errorf("pool-b = %+v, want it untouched", member)
	}
	for _, call := range w.provider.Calls()[calls:] {
		if strings.Contains(call, "pool-b") || strings.HasPrefix(call, "create ") || strings.HasPrefix(call, "wake ") {
			t.Errorf("the failed claim went on to %q", call)
		}
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("a failed claim read the key: %v", err)
	}
	if fills.Load() != 1 {
		t.Errorf("fills = %d, want the one started after the claim", fills.Load())
	}
	w.free("pool-a")
	w.free("lane-a")
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), w.contender(), runner, io.Discard, "lane-a", "")
	}, "lane lane-a already claimed warm workspace pool-a, now failed")
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.warm(t.Context(), w.contender(), runner, io.Discard, "pool-b", "")
	}, "pool-b names a warm pool workspace")
}

func TestAClaimedSpareWhoseCheckoutCannotBeReadIsNotTreatedAsClean(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	standInFill(t)
	if _, err := openPool(w.session).fill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.root, ".git", "index"), []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	calls, scripts := len(w.provider.Calls()), len(w.local.Scripts("pool-a"))
	native := 0
	runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
		native++
		return nil, errors.New("an unread checkout reached the Orca CLI")
	})
	err := w.warm(t.Context(), w.session, runner, io.Discard, "lane-a", "")
	if err == nil || !strings.Contains(err.Error(), "check the unused checkout on pool-a exited 128: its remote output is withheld") {
		t.Fatalf("warm = %v, want the failed git status reported without its output", err)
	}
	if native != 0 {
		t.Errorf("%d Orca calls after the failed check", native)
	}
	for _, call := range w.provider.Calls()[calls:] {
		if strings.HasPrefix(call, "wake ") || strings.HasPrefix(call, "create ") {
			t.Errorf("the failed check went on to %q", call)
		}
	}
	for _, script := range w.local.Scripts("pool-a")[scripts:] {
		if strings.Contains(script, "fetch --quiet") || strings.Contains(script, "checkout --quiet --force") {
			t.Errorf("the failed check went on to refresh the checkout: %q", script)
		}
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("the failed check read the key: %v", err)
	}
	for _, name := range []string{"pool-a", "lane-a"} {
		if _, err := os.Lstat(w.session.Config.State().Orca(name)); !os.IsNotExist(err) {
			t.Errorf("the failed check left Orca task state for %s: %v", name, err)
		}
	}
	if member := w.member("pool-a"); member.State != memberFailed || member.Lane != "lane-a" || member.Failure == nil {
		t.Errorf("pool-a = %+v, want it failed for lane-a", member)
	}
	w.free("pool-a")
	w.free("lane-a")
}

func TestThePublicFillWithholdsAnUnansweredLookupOfAReadySpare(t *testing.T) {
	dir := t.TempDir()
	path, calls := spritesConfig(t, dir, spriteFails)
	session, err := (&selection{config: path}).open()
	if err != nil {
		t.Fatal(err)
	}
	pool := openPool(session)
	record := &workspace.Record{Name: "pool-a", Provider: "sprites", Profile: "lean", Source: workspace.Source{Ref: "main"}, Machine: "pool-a"}
	if err := state.Save(session.Config.State().Workspace("pool-a"), record); err != nil {
		t.Fatal(err)
	}
	if err := pool.save(&orcaMember{SchemaVersion: orcaPoolSchema, Name: "pool-a", Provider: "sprites", Profile: "lean", Key: pool.key, State: memberReady, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	nameSpares(t)
	stdout, err := runPublicFill(t, path)
	if !strings.Contains(err.Error(), "the warm pool fill failed with exit 1 and HTTP status unknown") || !strings.Contains(err.Error(), "its text is withheld") {
		t.Fatalf("fill = %v, want the unanswered lookup reported as metadata", err)
	}
	var fill orcaFill
	if jsonErr := json.Unmarshal([]byte(stdout), &fill); jsonErr != nil || fill.Key != pool.key || len(fill.Created) != 0 || len(fill.Refused) != 0 {
		t.Errorf("fill output = %s, %v; want nothing created or refused", stdout, jsonErr)
	}
	invoked, readErr := os.ReadFile(calls)
	if readErr != nil || len(invoked) == 0 || strings.Trim(strings.ReplaceAll(string(invoked), "api\n", ""), "\n") != "" {
		t.Errorf("provider CLI calls = %q, %v; want only read-only api lookups", invoked, readErr)
	}
	if got, err := pool.load("pool-a"); err != nil || got.State != memberReady || got.Lane != "" || got.Reason != "" || got.Failure != nil {
		t.Errorf("pool-a = %+v, %v; want it left ready and unclaimed", got, err)
	}
	if members, err := pool.records(); err != nil || len(members) != 1 {
		t.Errorf("pool records = %+v, %v; want no new spare", members, err)
	}
	if _, err := os.Lstat(session.Config.State().Orca("pool-a")); !os.IsNotExist(err) {
		t.Errorf("the fill left Orca task state: %v", err)
	}
}

func TestWarmClaimRefusesUnverifiedOrIncompatibleSparesWithoutTouchingThem(t *testing.T) {
	record := func(w *firstWorker, edit func(*workspace.Record)) {
		w.t.Helper()
		loaded := &workspace.Record{}
		if _, err := state.Load(w.session.Config.State().Workspace("pool-a"), loaded); err != nil {
			w.t.Fatal(err)
		}
		edit(loaded)
		if err := state.Save(w.session.Config.State().Workspace("pool-a"), loaded); err != nil {
			w.t.Fatal(err)
		}
	}
	replace := func(w *firstWorker, labels map[string]string) {
		w.t.Helper()
		if err := w.provider.Destroy(w.t.Context(), "pool-a"); err != nil {
			w.t.Fatal(err)
		}
		if _, err := w.provider.Create(w.t.Context(), providers.Spec{Name: "pool-a", Labels: labels}); err != nil {
			w.t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		edit  func(*firstWorker, *orcaPool)
		state memberState
		want  string
	}{
		{"another compatibility key", func(w *firstWorker, pool *orcaPool) {
			member := w.member("pool-a")
			member.Key = strings.Repeat("0", 64)
			if err := pool.save(member); err != nil {
				w.t.Fatal(err)
			}
		}, memberReady, ""},
		{"a recorded Orca task", func(w *firstWorker, _ *orcaPool) {
			if err := state.Save(w.session.Config.State().Orca("pool-a"), &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "pool-a"}); err != nil {
				w.t.Fatal(err)
			}
		}, memberRefused, "an Orca task is already recorded"},
		{"no workspace record", func(w *firstWorker, _ *orcaPool) {
			if err := os.Remove(w.session.Config.State().Workspace("pool-a")); err != nil {
				w.t.Fatal(err)
			}
		}, memberRefused, "its workspace record is missing, unreadable, or another provider's or profile's"},
		{"an unverified create", func(w *firstWorker, _ *orcaPool) {
			record(w, func(r *workspace.Record) { r.Unverified = true })
		}, memberRefused, "never confirmed a machine"},
		{"a record of another name", func(w *firstWorker, _ *orcaPool) {
			record(w, func(r *workspace.Record) { r.Name = "pool-b" })
		}, memberRefused, "its workspace record names pool-b, not pool-a"},
		{"a record of another profile", func(w *firstWorker, _ *orcaPool) {
			record(w, func(r *workspace.Record) { r.Profile = "other" })
		}, memberRefused, "its workspace record is missing, unreadable, or another provider's or profile's"},
		{"a missing machine", func(w *firstWorker, _ *orcaPool) {
			if err := w.provider.Destroy(w.t.Context(), "pool-a"); err != nil {
				w.t.Fatal(err)
			}
		}, memberRefused, "fake machine pool-a is gone"},
		{"a same-name machine without the pool label", func(w *firstWorker, _ *orcaPool) {
			replace(w, map[string]string{workspace.LabelWorkspace: "pool-a"})
		}, memberRefused, "does not carry the ownership labels of warm workspace pool-a"},
		{"a machine of another pool", func(w *firstWorker, _ *orcaPool) {
			replace(w, map[string]string{workspace.LabelWorkspace: "pool-a", workspace.LabelPool: strings.Repeat("1", 64)})
		}, memberRefused, "does not carry the ownership labels of warm workspace pool-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWarmPool(t)
			pool := openPool(w.session)
			w.spare(pool, "pool-a")
			tt.edit(w, pool)
			calls := len(w.provider.Calls())
			if member, release, err := pool.claim(t.Context(), "lane-a", "main"); err != nil || member != nil || release != nil {
				t.Fatalf("claim = %+v, %v; want no spare", member, err)
			}
			if got := w.member("pool-a"); got.State != tt.state || !strings.Contains(got.Reason, tt.want) || got.Lane != "" || got.Failure != nil {
				t.Errorf("pool-a = %+v, want %s with %q", got, tt.state, tt.want)
			}
			if after := w.provider.Calls()[calls:]; len(after) != 0 {
				t.Errorf("the refused spare was touched: %q", after)
			}
			if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
				t.Errorf("a refusal read the key: %v", err)
			}
			if _, err := os.Lstat(w.session.Config.State().Orca("pool-b")); !os.IsNotExist(err) {
				t.Errorf("a refusal wrote Orca task state under another name: %v", err)
			}
			w.free("pool-a")
			if again, _, err := pool.claim(t.Context(), "lane-b", "main"); err != nil || again != nil {
				t.Errorf("a later claim = %+v, %v; want the refused spare kept out", again, err)
			}
		})
	}
}

func TestWarmClaimFailsWithoutClaimingWhenTheProviderCannotAnswer(t *testing.T) {
	w := newWarmPool(t)
	pool := openPool(w.session)
	w.spare(pool, "pool-a")
	session := w.over(&observing{Fake: w.provider, on: "pool-a", err: errors.New("sprites api answered 502")})
	member, _, err := openPool(session).claim(t.Context(), "lane-a", "main")
	if err == nil || member != nil || !strings.Contains(err.Error(), "observe warm workspace pool-a before claiming it: sprites api answered 502") {
		t.Fatalf("claim = %+v, %v", member, err)
	}
	if got := w.member("pool-a"); got.State != memberReady || got.Lane != "" || got.Reason != "" || got.Failure != nil {
		t.Errorf("pool-a = %+v, want it still unclaimed", got)
	}
	w.free("pool-a")
}

func TestWarmPoolNeverOffersAClaimedFinishedFailedOrUnfinishedSpareAgain(t *testing.T) {
	w := newWarmPool(t, "pool-new")
	pool := openPool(w.session)
	states := map[string]memberState{
		"pool-claimed":      memberClaimed,
		"pool-prepared":     memberPrepared,
		"pool-failed":       memberFailed,
		"pool-abandoned":    memberAbandoned,
		"pool-refused":      memberRefused,
		"pool-provisioning": memberProvisioning,
	}
	for name, kept := range states {
		w.spare(pool, name)
		member := w.member(name)
		member.State = kept
		if kept == memberClaimed {
			member.Lane = "lane-gone"
		}
		if err := pool.save(member); err != nil {
			t.Fatal(err)
		}
	}
	calls := len(w.provider.Calls())
	if member, _, err := pool.claim(t.Context(), "lane-a", "main"); err != nil || member != nil {
		t.Fatalf("claim = %+v, %v; want none of the used spares", member, err)
	}
	fill, err := pool.fill(t.Context())
	if err != nil || !slices.Equal(fill.Created, []string{"pool-new"}) || !slices.Equal(fill.Ready, []string{"pool-new"}) || !slices.Equal(fill.Abandoned, []string{"pool-provisioning"}) {
		t.Fatalf("fill = %+v, %v; want one new spare beside the abandoned one", fill, err)
	}
	for name, kept := range states {
		want := kept
		if kept == memberProvisioning {
			want = memberAbandoned
		}
		if got := w.member(name); got.State != want {
			t.Errorf("%s = %s, want %s", name, got.State, want)
		}
	}
	for _, call := range w.provider.Calls()[calls:] {
		if call != "create pool-new" && !strings.HasPrefix(call, "exec pool-new ") {
			t.Errorf("the claim or fill touched a used spare: %q", call)
		}
	}
	if err := w.provider.Destroy(t.Context(), "pool-claimed"); err != nil {
		t.Fatal(err)
	}
	if got := w.member("pool-claimed"); got.State != memberClaimed || got.Lane != "lane-gone" {
		t.Errorf("pool-claimed = %+v, want its claim kept without an observation", got)
	}
}

func TestWarmFillCreatesAgentFreeSparesWithoutAModelRuntimeOrKey(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	pool := openPool(w.session)
	fill, err := pool.fill(t.Context())
	if err != nil || fill.Key != pool.key || fill.Target != 1 || !slices.Equal(fill.Created, []string{"pool-a"}) {
		t.Fatalf("fill = %+v, %v", fill, err)
	}
	member := w.member("pool-a")
	if member.State != memberReady || member.Key != pool.key || member.Provider != "fake" || member.Profile != "lean" || member.Lane != "" {
		t.Errorf("pool record = %+v", member)
	}
	machine, err := w.provider.Get(t.Context(), "pool-a")
	if err != nil || machine.Labels[workspace.LabelWorkspace] != "pool-a" || machine.Labels[workspace.LabelPool] != pool.key {
		t.Errorf("machine = %+v, %v; want the workspace and pool labels", machine, err)
	}
	if _, err := w.session.Recorded("pool-a"); err != nil {
		t.Errorf("the spare has no workspace record: %v", err)
	}
	if _, err := os.Stat(w.keys); !os.IsNotExist(err) {
		t.Errorf("a fill read the key: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(w.session.Config.State().Orca("x"))); !os.IsNotExist(err) && (err != nil || len(entries) != 1 || entries[0].Name() != "pool-a.json.lock" || !entries[0].Type().IsRegular()) {
		t.Errorf("a fill wrote Orca task state: %v, %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(w.local.Home("pool-a"), ".cc-remote", "orca")); !os.IsNotExist(err) {
		t.Errorf("a fill left Orca runtime state on the spare: %v", err)
	}
	for _, script := range w.local.Scripts("pool-a") {
		if serveCommand.MatchString(script) || strings.Contains(script, "mkfifo") {
			t.Errorf("a fill started a runtime or a key pipe: %q", script)
		}
	}
	if got := sshCalls(t); len(got) != 0 {
		t.Errorf("ssh calls = %q", got)
	}
	again, err := pool.fill(t.Context())
	if err != nil || len(again.Created) != 0 || !slices.Equal(again.Ready, []string{"pool-a"}) {
		t.Errorf("second fill = %+v, %v; want the ready spare counted", again, err)
	}
	creates := 0
	for _, call := range w.provider.Calls() {
		if strings.HasPrefix(call, "create ") {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("creates = %d, want 1", creates)
	}
}

func TestConcurrentWarmFillsStopAtTheReadyTarget(t *testing.T) {
	w := newWarmPool(t, "pool-a", "pool-b")
	creating := newGate()
	w.paused = creating
	first, second := make(chan error, 1), make(chan error, 1)
	var filled, refilled *orcaFill
	go func() {
		var err error
		filled, err = openPool(w.session).fill(t.Context())
		first <- err
	}()
	w.reach(creating, first)
	go func() {
		var err error
		refilled, err = openPool(w.contender()).fill(t.Context())
		second <- err
	}()
	close(creating.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(filled.Created, []string{"pool-a"}) || !slices.Equal(filled.Ready, []string{"pool-a"}) || filled.Coalesced || len(refilled.Created) != 0 || !refilled.Coalesced {
		t.Errorf("fills = %+v and %+v, want one spare made and the later fill coalesced into the running one", filled, refilled)
	}
	creates := 0
	for _, call := range w.provider.Calls() {
		if strings.HasPrefix(call, "create ") {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("creates = %d, want 1 for a target of 1", creates)
	}
}

func TestAFillRecountsSparesClaimedWhileItCreates(t *testing.T) {
	w := newWarmPool(t, "pool-b", "pool-c")
	*w.session.Config.Orca.Pool.Ready = 2
	pool := openPool(w.session)
	w.spare(pool, "pool-a")
	creating := newGate()
	w.paused = creating
	done := make(chan error, 1)
	var fill *orcaFill
	go func() {
		var err error
		fill, err = pool.fill(t.Context())
		done <- err
	}()
	w.reach(creating, done)
	claimed, release, err := openPool(w.contender()).claim(t.Context(), "lane-a", "main")
	if err != nil || claimed == nil || claimed.Name != "pool-a" {
		t.Fatalf("claim during the fill = %+v, %v; want pool-a without waiting for the create", claimed, err)
	}
	release()
	close(creating.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fill.Created, []string{"pool-b", "pool-c"}) || !slices.Equal(fill.Ready, []string{"pool-b", "pool-c"}) {
		t.Errorf("fill = %+v, want pool-a's claim counted and the target refilled with pool-b and pool-c", fill)
	}
	for name, want := range map[string]memberState{"pool-a": memberClaimed, "pool-b": memberReady, "pool-c": memberReady} {
		if got := w.member(name); got.State != want {
			t.Errorf("%s = %s, want %s", name, got.State, want)
		}
	}
	w.free("pool-a")
}

func TestTheDetachedFillGetsNoModelCredentials(t *testing.T) {
	keys := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY"}
	if got := slices.Sorted(slices.Values(orca.CredentialEnv())); !slices.Equal(got, keys) {
		t.Fatalf("CredentialEnv = %q, want %q", got, keys)
	}
	environ := []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-synthetic-a", "GH_TOKEN=synthetic-git", "OPENAI_API_KEY=sk-synthetic-b", "ORCA_TERMINAL_HANDLE=term-synthetic", "CLAUDE_CODE_OAUTH_TOKEN=synthetic-c", "ANTHROPIC_AUTH_TOKEN=synthetic-d", "CODEX_API_KEY=synthetic-e", "SPRITE_PROVIDER_PROBE=kept"}
	want := []string{"PATH=/usr/bin", "GH_TOKEN=synthetic-git", "ORCA_TERMINAL_HANDLE=term-synthetic", "SPRITE_PROVIDER_PROBE=kept"}
	if got := fillEnv(environ); !slices.Equal(got, want) {
		t.Errorf("fillEnv = %q, want %q", got, want)
	}
	if len(environ) != 9 || environ[1] != "ANTHROPIC_API_KEY=sk-synthetic-a" {
		t.Errorf("fillEnv changed its input: %q", environ)
	}
	if got := fillEnv(nil); got == nil || len(got) != 0 {
		t.Errorf("fillEnv(nil) = %#v, want an explicit empty environment", got)
	}
	for _, key := range keys {
		t.Setenv(key, "sk-synthetic-"+strings.ToLower(key))
	}
	t.Setenv("SPRITE_PROVIDER_PROBE", "synthetic-provider")
	session := &workspace.Session{Config: &config.Config{Path: "/config/cc-remote.yaml"}, Kind: "sprites", Profile: "lean", Log: slog.New(slog.DiscardHandler)}
	pool := &orcaPool{session: session, dir: state.Dir(t.TempDir()), key: strings.Repeat("a", 64), target: 1}
	marker := filepath.Join(t.TempDir(), "names")
	original := fillCommand
	t.Cleanup(func() { fillCommand = original })
	fillCommand = func(string, string, string) (*exec.Cmd, error) {
		probe := `for name in PATH SPRITE_PROVIDER_PROBE ` + strings.Join(keys, " ") + `; do if printenv "$name" > /dev/null; then echo "$name"; fi; done > "$0.tmp"; mv "$0.tmp" "$0"`
		return exec.Command("sh", "-c", probe, marker), nil
	}
	if replenish := pool.replenish(); replenish.PID == 0 || replenish.Failure != nil {
		t.Fatalf("replenish = %+v", replenish)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		listed, err := os.ReadFile(marker)
		if err == nil {
			if string(listed) != "PATH\nSPRITE_PROVIDER_PROBE\n" {
				t.Errorf("the fill saw %q set, want PATH and the provider probe without any model key", listed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fill never reported its environment: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAFillCountsOnlySparesThatAreStillEligible(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(*firstWorker) func()
		refused bool
	}{
		{"a machine that is gone", func(w *firstWorker) func() {
			if err := w.provider.Destroy(w.t.Context(), "pool-a"); err != nil {
				w.t.Fatal(err)
			}
			return func() {}
		}, true},
		{"a recorded Orca task", func(w *firstWorker) func() {
			if err := state.Save(w.session.Config.State().Orca("pool-a"), &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "pool-a"}); err != nil {
				w.t.Fatal(err)
			}
			return func() {}
		}, true},
		{"a spare another command holds", func(w *firstWorker) func() {
			unlock, held, err := state.TryLock(w.session.Config.State().Orca("pool-a") + ".lock")
			if err != nil || !held {
				w.t.Fatalf("hold pool-a: %t, %v", held, err)
			}
			return unlock
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWarmPool(t, "pool-new")
			pool := openPool(w.session)
			w.spare(pool, "pool-a")
			release := tt.edit(w)
			calls := len(w.provider.Calls())
			fill, err := pool.fill(t.Context())
			release()
			if err != nil || !slices.Equal(fill.Created, []string{"pool-new"}) || !slices.Equal(fill.Ready, []string{"pool-new"}) {
				t.Fatalf("fill = %+v, %v; want pool-a left uncounted and pool-new made", fill, err)
			}
			want, refused := memberReady, []string(nil)
			if tt.refused {
				want, refused = memberRefused, []string{"pool-a"}
			}
			if got := w.member("pool-a"); got.State != want || !slices.Equal(fill.Refused, refused) {
				t.Errorf("pool-a = %+v, refused %q; want %s", got, fill.Refused, want)
			}
			for _, call := range w.provider.Calls()[calls:] {
				if strings.Contains(call, "pool-a") {
					t.Errorf("the fill touched pool-a: %q", call)
				}
			}
		})
	}
}

func spritesConfig(t *testing.T, dir, script string) (path, calls string) {
	t.Helper()
	cli, calls := filepath.Join(dir, "sprite"), filepath.Join(dir, "calls")
	t.Setenv("SPRITE_CALLS", calls)
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$SPRITE_CALLS\"\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(workspacetest.Inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf("repository: https://github.com/example/app\nref: main\nprovider: sprites\nprofile: lean\nstate_dir: %s\nproviders:\n  sprites: { org: test, cli: %s }\nworkspace_dirs:\n  sprites: /home/sprite\nprofiles:\n  lean: {}\ninventory: ./inventory.yaml\nforwards:\n  - { label: web, env: WEB_PORT }\n", filepath.Join(dir, "state"), cli)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func runPublicFill(t *testing.T, path string) (string, error) {
	t.Helper()
	var logged bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(original)
	cmd := NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetArgs([]string{"orca", "pool", "fill", "--config", path})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("the public fill succeeded against a failing provider")
	}
	for stream, text := range map[string]string{"error": err.Error(), "stdout": stdout.String(), "stderr": stderr.String(), "log": logged.String()} {
		for _, leaked := range []string{"sk-leakedsecret", "fatal", "synthetic provider"} {
			if strings.Contains(text, leaked) {
				t.Errorf("the public fill's %s carried %q: %s", stream, leaked, text)
			}
		}
	}
	return stdout.String(), err
}

func TestThePublicFillRecordsOnlyTheFailedStepAndMetadata(t *testing.T) {
	const absent = `printf '{"error":"sprite not found"}\n404'` + "\n"
	tests := []struct {
		name    string
		script  string
		want    string
		failure string
		calls   string
	}{
		{
			"a lookup the CLI cannot make", spriteFails,
			"exit 1 and HTTP status unknown at create.preflight", `{"step":"create.preflight","exit":1,"httpStatus":null,"bytes":0,"sha256":""}`, "api\n",
		},
		{
			"a lookup the API refuses", `printf '{"error":"fatal: sk-leakedsecret answered by the synthetic provider"}\n503'` + "\n",
			"exit unknown and HTTP status 503 at create.preflight", `{"step":"create.preflight","exit":null,"httpStatus":503,"bytes":0,"sha256":""}`, "api\n",
		},
		{
			"a create the CLI rejects", `if [ "$1" = create ]; then echo 'fatal: sk-leakedsecret answered by the synthetic provider' >&2; exit 1; fi` + "\n" + absent,
			"exit 1 and HTTP status unknown at create.command", `{"step":"create.command","exit":1,"httpStatus":null,"bytes":0,"sha256":""}`, "api\ncreate\napi\n",
		},
		{
			"a readback the API refuses", `if [ "$1" = create ]; then : > "$SPRITE_CALLS.created"; exit 0; fi` + "\n" + `if [ -e "$SPRITE_CALLS.created" ]; then printf '{"error":"fatal: sk-leakedsecret answered by the synthetic provider"}\n500'; exit 0; fi` + "\n" + absent,
			"exit unknown and HTTP status 500 at create.readback", `{"step":"create.readback","exit":null,"httpStatus":500,"bytes":0,"sha256":""}`, "api\ncreate\napi\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path, calls := spritesConfig(t, dir, tt.script)
			nameSpares(t, "pool-a")
			_, err := runPublicFill(t, path)
			if !strings.Contains(err.Error(), "create warm workspace pool-a failed with "+tt.want+";") {
				t.Fatalf("fill = %v, want the failed create reported as %q", err, tt.want)
			}
			states := state.Dir(filepath.Join(dir, "state"))
			member := &orcaMember{}
			if _, err := state.Load(states.Pool("pool-a"), member); err != nil || member.State != memberFailed || member.Failure == nil || member.Failure.Bytes == 0 || len(member.Failure.SHA256) != 64 {
				t.Fatalf("pool-a = %+v, %v; want it failed with error metadata", member, err)
			}
			member.Failure.Bytes, member.Failure.SHA256 = 0, ""
			if failure, err := json.Marshal(member.Failure); err != nil || string(failure) != tt.failure {
				t.Errorf("failure = %s, %v; want %s", failure, err, tt.failure)
			}
			record := &workspace.Record{}
			if _, err := state.Load(states.Workspace("pool-a"), record); err != nil || !record.Unverified || record.Machine != "pool-a" {
				t.Errorf("workspace pool-a = %+v, %v; want its record kept unverified", record, err)
			}
			for _, stored := range []string{states.Pool("pool-a"), states.Workspace("pool-a")} {
				raw, err := os.ReadFile(stored)
				if err != nil {
					t.Fatal(err)
				}
				for _, leaked := range []string{"sk-leakedsecret", "fatal", "synthetic provider", "sprite create"} {
					if bytes.Contains(raw, []byte(leaked)) {
						t.Errorf("%s kept %q: %s", stored, leaked, raw)
					}
				}
			}
			if invoked, err := os.ReadFile(calls); err != nil || string(invoked) != tt.calls {
				t.Errorf("provider CLI calls = %q, %v; want %q with no retry or destroy", invoked, err, tt.calls)
			}
		})
	}
}

func TestAFillKeepsItsReadySpareWhenTheNextCreateIsRejected(t *testing.T) {
	w := newWarmPool(t, "pool-a", "pool-b")
	*w.session.Config.Orca.Pool.Ready = 2
	provider := &rejecting{Fake: w.provider, name: "pool-b"}
	session := w.over(provider)
	session.Token = w.session.Token
	session.Stderr = io.Discard
	pool := openPool(session)
	fill, err := pool.fill(t.Context())
	if err == nil || !strings.Contains(err.Error(), "create warm workspace pool-b failed with exit 1 and HTTP status unknown at create.command;") || strings.Contains(err.Error(), "sk-leakedsecret") || !slices.Equal(fill.Created, []string{"pool-a"}) {
		t.Fatalf("fill = %+v, %v; want pool-a made and pool-b's rejected create reported as metadata", fill, err)
	}
	if got := w.member("pool-a"); got.State != memberReady {
		t.Errorf("pool-a = %+v, want it kept ready", got)
	}
	failed := w.member("pool-b")
	if failed.State != memberFailed || failed.Failure == nil || failed.Failure.Step != sprites.StepCommand || failed.Failure.Exit == nil || *failed.Failure.Exit != 1 || failed.Failure.HTTPStatus != nil {
		t.Errorf("pool-b = %+v, want it failed at the create command with exit 1", failed)
	}
	if stored, err := os.ReadFile(w.session.Config.State().Pool("pool-b")); err != nil || bytes.Contains(stored, []byte("sk-leakedsecret")) || bytes.Contains(stored, []byte("fatal")) || bytes.Contains(stored, []byte("sprites.dev")) {
		t.Errorf("the pool record kept provider output: %s, %v", stored, err)
	}
	record := &workspace.Record{}
	if _, err := state.Load(w.session.Config.State().Workspace("pool-b"), record); err != nil || !record.Unverified || record.Machine != "pool-b" {
		t.Errorf("workspace pool-b = %+v, %v; want its record kept unverified", record, err)
	}
	if ready, err := pool.ready(); err != nil || ready != 1 {
		t.Errorf("ready = %d, %v; want pool-a alone", ready, err)
	}
	creates := 0
	for _, call := range w.provider.Calls() {
		if strings.HasPrefix(call, "create ") {
			creates++
		}
		if strings.HasPrefix(call, "destroy ") {
			t.Errorf("the fill destroyed a machine after the rejected create: %q", call)
		}
	}
	if creates != 1 || provider.creates.Load() != 1 {
		t.Errorf("creates = %d made and %d rejected, want one of each and no retry", creates, provider.creates.Load())
	}
}

func TestWarmFillRecordsAFailedCreateAndStops(t *testing.T) {
	w := newWarmPool(t, "pool-a", "pool-b")
	*w.session.Config.Orca.Pool.Ready = 2
	w.provider.Handle = func(id string, cmd []string, stdin []byte) providers.Result {
		if strings.Contains(cmd[len(cmd)-1], "clone --quiet") {
			return providers.Result{ExitCode: 1, Stderr: []byte("fatal: sk-leakedsecret could not read the origin\n")}
		}
		return w.handle(id, cmd, stdin)
	}
	pool := openPool(w.session)
	fill, err := pool.fill(t.Context())
	if err == nil || !strings.Contains(err.Error(), "create warm workspace pool-a failed with exit unknown and HTTP status unknown") || strings.Contains(err.Error(), "sk-leakedsecret") || strings.Contains(err.Error(), "fatal") || len(fill.Created) != 0 || len(fill.Ready) != 0 {
		t.Fatalf("fill = %+v, %v; want the failed create reported as metadata", fill, err)
	}
	member := w.member("pool-a")
	if member.State != memberFailed || member.Failure == nil || member.Failure.Step != "" || member.Failure.Bytes == 0 || len(member.Failure.SHA256) != 64 {
		t.Errorf("pool-a = %+v, want it failed with error metadata and no provider create step", member)
	}
	if stored, err := os.ReadFile(w.session.Config.State().Pool("pool-a")); err != nil || bytes.Contains(stored, []byte("sk-leakedsecret")) || bytes.Contains(stored, []byte("fatal")) {
		t.Errorf("the pool record kept remote output: %s, %v", stored, err)
	}
	for _, call := range w.provider.Calls() {
		if strings.Contains(call, "pool-b") {
			t.Errorf("the fill tried another spare after a failure: %q", call)
		}
	}
	if !slices.Contains(w.provider.Calls(), "destroy pool-a") {
		t.Errorf("provider calls = %q, want the failed create to remove its machine", w.provider.Calls())
	}
	if loaded, err := pool.load("pool-b"); err != nil || loaded != nil {
		t.Errorf("pool-b = %+v, %v; want no record", loaded, err)
	}
	if claimed, _, err := pool.claim(t.Context(), "lane-a", "main"); err != nil || claimed != nil {
		t.Errorf("claim = %+v, %v; want the failed spare kept out", claimed, err)
	}
}

func TestReplenishStartsOneDetachedFillAndReturnsAtOnce(t *testing.T) {
	session := &workspace.Session{Config: &config.Config{Path: "/config/cc-remote.yaml"}, Kind: "sprites", Profile: "lean", Log: slog.New(slog.DiscardHandler)}
	pool := &orcaPool{session: session, dir: state.Dir(t.TempDir()), key: strings.Repeat("a", 64), target: 1}
	marker := filepath.Join(t.TempDir(), "fill")
	var argv []string
	original := fillCommand
	t.Cleanup(func() { fillCommand = original })
	fillCommand = func(config, provider, profile string) (*exec.Cmd, error) {
		argv = []string{config, provider, profile}
		return exec.Command("sh", "-c", `printf '%s %s\n' "$$" "$(ps -o pgid= -p $$ | tr -d ' ')" > "$0.tmp"; cat; echo "stdin closed" >> "$0.tmp"; mv "$0.tmp" "$0"; sleep 3`, marker), nil
	}
	started := time.Now()
	replenish := pool.replenish()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("replenish waited %s for its fill", elapsed)
	}
	if replenish.Target != 1 || replenish.PID == 0 || replenish.Failure != nil || !slices.Equal(argv, []string{"/config/cc-remote.yaml", "sprites", "lean"}) {
		t.Fatalf("replenish = %+v for %q", replenish, argv)
	}
	want := fmt.Sprintf("%d %d\nstdin closed\n", replenish.PID, replenish.PID)
	deadline := time.Now().Add(30 * time.Second)
	for {
		marked, err := os.ReadFile(marker)
		if err == nil && string(marked) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fill marker = %q, %v; want %q from a session of its own reading the null device", marked, err, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fillCommand = func(string, string, string) (*exec.Cmd, error) {
		return nil, errors.New("no helper binary")
	}
	if failed := pool.replenish(); failed.PID != 0 || failed.Failure == nil || failed.Failure.Bytes != len("no helper binary") || failed.Failure.Exit != nil {
		t.Errorf("replenish = %+v, want the failed start reported as metadata", failed)
	}
	pool.target = 0
	fillCommand = func(string, string, string) (*exec.Cmd, error) {
		t.Error("a pool with no ready target started a fill")
		return nil, errors.New("unexpected")
	}
	if disabled := pool.replenish(); *disabled != (orcaReplenish{}) {
		t.Errorf("replenish = %+v, want nothing started", disabled)
	}
}

func TestFailureKeepsOnlyErrorMetadata(t *testing.T) {
	secret := providers.Result{ExitCode: 7, Stderr: []byte("sk-secret https://api.sprites.dev/v1/sprites?token=sk-secret {\"body\":1}")}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"a command", fmt.Errorf("check: %w", &providers.CommandError{Command: "sh -c on pool-a", Result: secret}), `{"exit":7,"httpStatus":null,"bytes":0,"sha256":""}`},
		{"plain text", errors.New("sprites api answered 502: body"), `{"exit":null,"httpStatus":null,"bytes":0,"sha256":""}`},
		{"a rejected create", &sprites.CreateError{Step: sprites.StepCommand, Err: fmt.Errorf("sprite pool-a: whether the create allocated it is unknown: %w", &providers.CommandError{Command: "sprite create -o org --skip-console pool-a", Result: secret})}, `{"step":"create.command","exit":7,"httpStatus":null,"bytes":0,"sha256":""}`},
		{"a refused readback", fmt.Errorf("create pool-a: %w", &sprites.CreateError{Step: sprites.StepReadback, Err: fmt.Errorf("%w for pool-a: %s", &sprites.StatusError{Status: 502}, secret.Stderr)}), `{"step":"create.readback","exit":null,"httpStatus":502,"bytes":0,"sha256":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := failureOf(tt.err)
			raw, err := json.Marshal(failure)
			if err != nil || failure.Bytes != len(tt.err.Error()) || len(failure.SHA256) != 64 {
				t.Fatalf("failureOf(%v) = %s, %v", tt.err, raw, err)
			}
			for _, leaked := range []string{"secret", "body", "sprites.dev", "token", "pool-a"} {
				if bytes.Contains(raw, []byte(leaked)) {
					t.Errorf("failureOf kept %q: %s", leaked, raw)
				}
			}
			failure.Bytes, failure.SHA256 = 0, ""
			if metadata, err := json.Marshal(failure); err != nil || string(metadata) != tt.want {
				t.Errorf("failureOf(%v) = %s, %v; want %s", tt.err, metadata, err, tt.want)
			}
		})
	}
}

func TestWarmRefusesWhatThePoolCannotKeep(t *testing.T) {
	if err := warmable(&workspace.Session{Kind: "namespace"}); err == nil || err.Error() != "the warm pool keeps only sprites workspaces, not namespace ones" {
		t.Errorf("namespace = %v", err)
	}
	fakeRemote(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fills := standInFill(t)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("do the work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	example := filepath.Join("..", "..", "examples", "config.yaml")
	calls := 0
	runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("no runtime operation is expected")
	})
	for _, cmd := range []*cobra.Command{newOrcaPrepareCmd(runner), newOrcaPoolFillCmd(), newOrcaPoolStatusCmd()} {
		args := []string{"--config", example, "--provider", "sprites", "--profile", "full"}
		if cmd.Name() == "prepare" {
			args = append([]string{"lane-a", "--warm", "--model", "claude-opus-5-5", "--effort", "xhigh", "--brief-file", brief}, args...)
		}
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "profile full runs prepare steps") {
			t.Errorf("%s = %v, want the profile refused", cmd.Name(), err)
		}
	}
	if calls != 0 || fills.Load() != 0 {
		t.Errorf("a refusal made %d Orca calls and started %d fills", calls, fills.Load())
	}
	if got := sshCalls(t); len(got) != 0 {
		t.Errorf("ssh calls = %q", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "cc-remote", "pool")); !os.IsNotExist(err) {
		t.Errorf("a refusal wrote pool state: %v", err)
	}
}
