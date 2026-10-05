package cli

import (
	"bytes"
	"context"
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
	"github.com/yasyf/cc-remote/internal/remote"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
	"github.com/yasyf/cc-remote/internal/workspace/workspacetest"
)

const standInHold = 20 * time.Second

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "hold-gateway" {
		os.Exit(holdGatewayStandIn(os.Args[2], os.Args[3]))
	}
	fillCommand = func(string, string, string) (*exec.Cmd, error) {
		return nil, fmt.Errorf("a test started the warm pool fill without a stand-in")
	}
	os.Exit(m.Run())
}

func holdGatewayStandIn(lock, hold string) int {
	duration, err := time.ParseDuration(hold)
	if err != nil {
		fmt.Println(err)
		return 2
	}
	unlock, err := holdGateway(lock)
	if err != nil {
		fmt.Println(err)
		return 3
	}
	defer unlock()
	time.Sleep(duration)
	return 0
}

func standInForward(t *testing.T, lock *string) *atomic.Int32 {
	t.Helper()
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	starts := &atomic.Int32{}
	original := forwardCommand
	forwardCommand = func(string, string) (*exec.Cmd, error) {
		starts.Add(1)
		return exec.Command(helper, "hold-gateway", *lock, standInHold.String()), nil
	}
	t.Cleanup(func() { forwardCommand = original })
	return starts
}

func refuseForward(t *testing.T) {
	t.Helper()
	original := forwardCommand
	forwardCommand = func(workspace, _ string) (*exec.Cmd, error) {
		t.Errorf("a second forward was started for %s", workspace)
		return nil, fmt.Errorf("no second forward")
	}
	t.Cleanup(func() { forwardCommand = original })
}

func computeSession(t *testing.T, provider providers.Provider, root string) *workspace.Session {
	t.Helper()
	return &workspace.Session{
		Config:   &config.Config{Path: "/config/cc-remote.yaml", StateDir: t.TempDir(), Repository: root, Roots: map[string]string{"fake": filepath.Dir(root)}},
		Kind:     "fake",
		Profile:  "lean",
		Provider: provider,
		Log:      slog.New(slog.DiscardHandler),
	}
}

func computeRecord(t *testing.T, provider *providertest.Fake) *workspace.Record {
	t.Helper()
	machine, err := provider.Create(t.Context(), providers.Spec{Name: "task-a"})
	if err != nil {
		t.Fatal(err)
	}
	return &workspace.Record{Name: "task-a", Provider: "fake", Profile: "lean", Machine: machine.ID, Compute: machine.Compute}
}

func computeFake(handle func(string, []string, []byte) providers.Result) *providertest.Fake {
	return &providertest.Fake{
		Compute: &providers.ComputeInstance{Container: "agent", Endpoint: "https://compute.test", ContainerPort: 18766, ExportedPort: 30000, IngressDomain: "us.test.nscluster.cloud"},
		Handle:  handle,
	}
}

func TestRetainStartsTheSoleKeeperOnceTheInstanceIsRecorded(t *testing.T) {
	provider := computeFake(nil)
	session := computeSession(t, provider, filepath.Join(t.TempDir(), "app"))
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	lock := driver.state.OrcaGatewayLock("task-a", "task-a")
	starts := standInForward(t, &lock)
	task, err := driver.retain(t.Context(), session, computeRecord(t, provider), orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if task.Gateway == nil || task.Gateway.Lock != lock || task.Gateway.Instance != "task-a" || task.Gateway.ContainerPort != 18766 || task.Forward != nil || task.Agent.Model != "m" {
		t.Fatalf("task = %+v, gateway %+v", task, task.Gateway)
	}
	if !task.up(t.Context()) || starts.Load() != 1 {
		t.Fatalf("keeper up = %t after %d starts, want the one keeper holding its lock", task.up(t.Context()), starts.Load())
	}
	loaded, err := loadTask(driver.state, "task-a")
	if err != nil || loaded.Port != task.Port || loaded.Gateway.PID == 0 || loaded.RuntimeID != "" || loaded.Terminal != "" {
		t.Errorf("saved task = %+v, %v; want the keeper recorded before any runtime or worker", loaded, err)
	}
	if err := task.ensure(t.Context(), session.Config.Path); err != nil || starts.Load() != 1 {
		t.Errorf("a later ensure = %v after %d starts, want the same keeper reused", err, starts.Load())
	}
	if got := provider.Calls(); !slices.Equal(got, []string{"create task-a"}) {
		t.Errorf("provider calls = %q, want no runtime, key, or prompt before provisioning", got)
	}
}

func TestRetainOnResumeRestartsAnAbsentKeeperForTheSavedTask(t *testing.T) {
	provider := computeFake(nil)
	session := computeSession(t, provider, filepath.Join(t.TempDir(), "app"))
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	record := computeRecord(t, provider)
	saved, err := driver.task(&workspace.Result{Name: "task-a", Provider: "fake", Profile: "lean", Machine: record.Machine, ProjectRoot: "/workspaces/app", Compute: record.Compute}, orca.Agent{}, provider)
	if err != nil {
		t.Fatal(err)
	}
	saved.RuntimeID = "rt-1"
	if err := driver.save(saved); err != nil {
		t.Fatal(err)
	}
	starts := standInForward(t, &saved.Gateway.Lock)
	task, err := driver.retain(t.Context(), session, record, orca.Agent{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || !task.up(t.Context()) || task.Port != saved.Port || task.RuntimeID != "rt-1" || task.provider != provider {
		t.Errorf("resumed task = %+v after %d starts, want the saved task's port and runtime with its keeper restarted", task, starts.Load())
	}
}

func TestRetainRefusesASavedTaskForAnotherInstance(t *testing.T) {
	provider := computeFake(nil)
	session := computeSession(t, provider, filepath.Join(t.TempDir(), "app"))
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	record := computeRecord(t, provider)
	other := *record.Compute
	other.InstanceID = "inst-old"
	if _, err := driver.task(&workspace.Result{Name: "task-a", Provider: "fake", Machine: "inst-old", Compute: &other}, orca.Agent{}, provider); err != nil {
		t.Fatal(err)
	}
	refuseForward(t)
	if _, err := driver.retain(t.Context(), session, record, orca.Agent{}, true); err == nil || err.Error() != "the saved Orca task for task-a does not forward instance task-a, so no keeper is started for it" {
		t.Errorf("retain = %v", err)
	}
}

func TestRetainLeavesAnSSHWorkspaceToItsTunnel(t *testing.T) {
	provider := &providertest.Fake{}
	session := computeSession(t, provider, filepath.Join(t.TempDir(), "app"))
	driver := orcaDriver{state: session.Config.State(), log: session.Log}
	refuseForward(t)
	task, err := driver.retain(t.Context(), session, &workspace.Record{Name: "task-a", Provider: "fake", Machine: "task-a"}, orca.Agent{}, false)
	if err != nil || task != nil {
		t.Errorf("retain = %+v, %v; want nothing retained", task, err)
	}
	if _, err := os.Stat(driver.state.Orca("task-a")); !os.IsNotExist(err) {
		t.Errorf("an SSH workspace got an Orca task before provisioning: %v", err)
	}
}

func TestEnsureReportsAForwardThatExitsBeforeHoldingItsListener(t *testing.T) {
	dir := state.Dir(t.TempDir())
	original := forwardCommand
	forwardCommand = func(string, string) (*exec.Cmd, error) {
		return exec.Command("sh", "-c", "echo 'hold 127.0.0.1:7001 for task-a: address already in use' >&2; exit 1"), nil
	}
	t.Cleanup(func() { forwardCommand = original })
	gateway := &orcaGateway{Instance: "inst1", ContainerPort: 18766, Lock: dir.OrcaGatewayLock("task-a", "inst1"), Log: dir.OrcaForwardLog("task-a")}
	err := gateway.ensure(t.Context(), "task-a", "/config/cc-remote.yaml", 7001)
	want := "the gateway forward for task-a on 127.0.0.1:7001 exited before it held the listener: exit status 1: hold 127.0.0.1:7001 for task-a: address already in use (see " + gateway.Log + ")"
	if err == nil || err.Error() != want {
		t.Errorf("ensure = %v, want %q", err, want)
	}
	if gateway.PID != 0 {
		t.Errorf("a forward that never held its listener was recorded as pid %d", gateway.PID)
	}
}

func computeReady(runtimeID string, loopback int) string {
	return fmt.Sprintf(`{"type":"orca_server_ready","schemaVersion":1,"runtimeId":%q,"boundEndpoint":"ws://0.0.0.0:18766","advertisedEndpoint":"ws://127.0.0.1:%d","pairing":{"available":true,"url":"orca://pair?code=private"}}`, runtimeID, loopback)
}

func viaFakeRemote(script string, stdin []byte) providers.Result {
	result, err := providers.OSRunner{}.Run(context.Background(), providers.Command{Name: "ssh", Args: []string{"task-a", "sh", "-c", remote.Quote(script)}, Stdin: bytes.NewReader(stdin)})
	if err != nil {
		return providers.Result{Stderr: []byte(err.Error()), ExitCode: 255}
	}
	return result
}

func TestComputeLaunchReachesItsRuntimeThroughTheRetainedGateway(t *testing.T) {
	agent := orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}
	runtime := orca.Runtime{Entry: "tools/orca/AppRun"}
	home := fakeRemote(t)
	root, head := gitCheckout(t)
	var loopback int
	var ensured string
	provider := computeFake(func(_ string, cmd []string, stdin []byte) providers.Result {
		if cmd[2] == ensured {
			return providers.Result{Stdout: []byte("starting\n" + computeReady("rt-1", loopback) + "\n")}
		}
		return viaFakeRemote(cmd[2], stdin)
	})
	session := computeSession(t, provider, root)
	envelope := func(runtimeID, result string) string {
		return `{"ok":true,"result":` + result + `,"_meta":{"runtimeId":"` + runtimeID + `"}}`
	}
	worktree := "repo-1::" + root
	scope := " --environment task-a --json"
	commands := []string{
		"environment add --name task-a --pairing-code orca://pair?code=private --json",
		"status" + scope,
		"repo add --path " + root + scope,
		"worktree list --repo id:repo-1" + scope,
		"terminal create --worktree id:" + worktree + " --title task-a --command " + agent.Command(fakeKeyDir) + scope,
		"terminal read --terminal term-1 --screen" + scope,
		"terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope,
	}
	fake := &scriptedOrca{t: t, replies: map[string]string{
		commands[0]: envelope("local", `{"environment":{"id":"env-1","name":"task-a"}}`),
		commands[1]: envelope("rt-1", `{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}}`),
		commands[2]: envelope("rt-1", `{"repo":{"id":"repo-1","path":"`+root+`"}}`),
		commands[3]: envelope("rt-1", `{"worktrees":[{"id":"`+worktree+`","repoId":"repo-1","path":"`+root+`"}]}`),
		commands[4]: envelope("rt-1", `{"terminal":{"handle":"term-1"}}`),
		commands[5]: envelope("rt-1", `{"terminal":{"handle":"term-1","status":"running","source":"screen","tail":["Opus 5.5 (xhigh) · API Usage Billing","> "]}}`),
		commands[6]: envelope("rt-1", `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null,"blockedReason":""}}`),
	}}
	driver := orcaDriver{client: orca.NewClient(fake), state: session.Config.State(), log: session.Log}
	lock := driver.state.OrcaGatewayLock("task-a", "task-a")
	starts := standInForward(t, &lock)
	task, err := driver.retain(t.Context(), session, computeRecord(t, provider), agent, false)
	if err != nil {
		t.Fatal(err)
	}
	loopback = task.Port
	gatewayRuntime := runtime
	gatewayRuntime.Port, gatewayRuntime.Advertise = 18766, fmt.Sprintf("ws://127.0.0.1:%d", loopback)
	ensured = gatewayRuntime.EnsureScript()
	if err := driver.launch(t.Context(), session, task, runtime, []byte("sk-test-key"), nil, "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := driver.publish(t.Context(), task, []byte("Read every line.\n")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant\n%q", fake.calls, commands)
	}
	if got := provider.Calls(); len(got) < 2 || got[1] != fmt.Sprintf("exec task-a %q", []string{"sh", "-c", ensured}) {
		t.Errorf("provider calls = %q, want the runtime on container port 18766 advertising loopback %d", got, loopback)
	}
	if got := sshCalls(t); !slices.Equal(got, []string{"key-dir", "key-write", "head", "brief"}) {
		t.Errorf("remote calls = %q, want the key pipe, head, and brief through the provider and no ssh check", got)
	}
	if starts.Load() != 1 {
		t.Errorf("forward starts = %d, want the retained keeper reused by the launch", starts.Load())
	}
	loaded, err := loadTask(driver.state, "task-a")
	if err != nil || loaded.RuntimeID != "rt-1" || !loaded.Prepared || loaded.BaseCommit != head || loaded.Port != loopback || loaded.Forward != nil {
		t.Errorf("stored task = %+v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a", "brief.md")); err != nil {
		t.Errorf("brief: %v", err)
	}
}

func TestServerResumeAdoptsOnlyTheSavedRuntime(t *testing.T) {
	tests := []struct {
		name  string
		probe func(loopback int) providers.Result
		want  string
	}{
		{"the saved runtime", func(loopback int) providers.Result {
			return providers.Result{Stdout: []byte(computeReady("rt-1", loopback) + "\n")}
		}, ""},
		{"a replaced runtime", func(loopback int) providers.Result {
			return providers.Result{Stdout: []byte(computeReady("rt-2", loopback) + "\n")}
		}, "task-a now answers as Orca runtime rt-2, not its saved rt-1; the other runtime is not adopted, and the saved task and every process are kept"},
		{"no runtime", func(int) providers.Result {
			return providers.Result{Stderr: []byte("cc-remote: no Orca runtime holds serve.lock\n"), ExitCode: 3}
		}, "task-a has no running Orca runtime rt-1 to resume (cc-remote: no Orca runtime holds serve.lock); none is started in its place, and the saved task is kept"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var loopback int
			provider := computeFake(func(_ string, cmd []string, _ []byte) providers.Result {
				if cmd[2] != orca.ProbeScript {
					t.Errorf("resume ran %q instead of only probing the runtime", cmd[2])
					return providers.Result{ExitCode: 1}
				}
				return tt.probe(loopback)
			})
			session := computeSession(t, provider, filepath.Join(t.TempDir(), "app"))
			driver := orcaDriver{state: session.Config.State(), log: session.Log}
			record := computeRecord(t, provider)
			task, err := driver.task(&workspace.Result{Name: "task-a", Provider: "fake", Profile: "lean", Machine: record.Machine, ProjectRoot: "/workspaces/app", Compute: record.Compute}, orca.Agent{}, provider)
			if err != nil {
				t.Fatal(err)
			}
			task.RuntimeID, loopback = "rt-1", task.Port
			if err := driver.save(task); err != nil {
				t.Fatal(err)
			}
			unlock, held, err := state.TryLock(task.Gateway.Lock)
			if err != nil || !held {
				t.Fatalf("TryLock = %t, %v", held, err)
			}
			defer unlock()
			refuseForward(t)
			before, err := os.ReadFile(driver.state.Orca("task-a"))
			if err != nil {
				t.Fatal(err)
			}
			pairing, err := driver.serve(t.Context(), session, task, orca.Runtime{Entry: "tools/orca/AppRun"}, true)
			switch {
			case tt.want == "" && (err != nil || pairing != "orca://pair?code=private"):
				t.Errorf("serve = %q, %v; want the saved runtime's pairing", pairing, err)
			case tt.want != "" && (err == nil || err.Error() != tt.want || pairing != ""):
				t.Errorf("serve = %q, %v; want %q", pairing, err, tt.want)
			}
			after, err := os.ReadFile(driver.state.Orca("task-a"))
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("the saved task changed on resume:\n%s\nwant\n%s", after, before)
			}
			if got := provider.Calls(); len(got) != 2 || !strings.HasPrefix(got[1], "exec task-a") {
				t.Errorf("provider calls = %q, want one probe", got)
			}
		})
	}
}

func TestExistingComputePrepareStartsItsKeeperEarlyUnderTheHeldTask(t *testing.T) {
	w := newFirstWorker(t, &providers.ComputeInstance{Container: "agent", Endpoint: "https://compute.test", ContainerPort: 18766, ExportedPort: 30000, IngressDomain: "us.test.nscluster.cloud"}, true)
	lock := w.session.Config.State().OrcaGatewayLock("task-a", "task-a")
	starts := standInForward(t, &lock)
	native, commands := w.native()
	calls := len(w.provider.Calls())
	resuming := newGate()
	w.paused = resuming
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- w.prepare(t.Context(), w.session, native, &out, "task-a", true) }()
	w.reach(resuming, done)
	early, err := loadTask(w.session.Config.State(), "task-a")
	if err != nil || early.Gateway == nil || early.Gateway.Instance != "task-a" || early.Gateway.PID == 0 || early.RuntimeID != "" || early.Terminal != "" || starts.Load() != 1 {
		t.Fatalf("task while resuming = %+v, %v after %d keeper starts; want the new task's keeper before any runtime", early, err, starts.Load())
	}
	contender := w.contender()
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.prepare(t.Context(), contender, runner, io.Discard, "task-a", true)
	}, "another cc-remote command holds the Orca task for task-a")
	close(resuming.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	loaded := w.prepared(out.Bytes())
	if loaded.Port != early.Port || loaded.Gateway == nil || loaded.Gateway.Instance != "task-a" || loaded.Forward != nil || loaded.RuntimeID != "rt-1" {
		t.Errorf("prepared compute task = %+v, want the early task's port and gateway", loaded)
	}
	if starts.Load() != 1 || !slices.Equal(native.calls, commands) {
		t.Errorf("keeper starts = %d, orca calls =\n%q\nwant one keeper and\n%q", starts.Load(), native.calls, commands)
	}
	if lifecycle := w.provider.Calls()[calls:]; len(lifecycle) == 0 || lifecycle[0] != "wake task-a" || slices.ContainsFunc(lifecycle, func(call string) bool {
		return strings.HasPrefix(call, "create ") || strings.HasPrefix(call, "destroy ")
	}) {
		t.Errorf("provider calls = %q, want the recorded instance woken and nothing created or destroyed", lifecycle)
	}
	w.free("task-a")
}

func recipeConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(workspacetest.Inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("repository: %s\nref: main\nprovider: sprites\nprofile: lean\nstate_dir: %q\nproviders:\n  sprites: {org: test, cli: /nonexistent/sprite}\nworkspace_dirs:\n  sprites: /home/sprite\nprofiles:\n  lean: {}\ninventory: ./inventory.yaml\nforwards:\n  - {label: web, env: WEB_PORT}\n", cfg.Repository, cfg.StateDir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerRecipesRefuseWhileAFirstWorkerHoldsTheTask(t *testing.T) {
	w := newFirstWorker(t, nil, true)
	path := recipeConfig(t, w.session.Config)
	for name, value := range map[string]string{envSchemaVersion: "2", envRepoURL: w.session.Config.Repository, envRepoRef: "main", envRepoRefHead: w.head, envRepoBranch: "main"} {
		t.Setenv(name, value)
	}
	native, commands := w.native()
	booting, runner := w.booting(native, commands[5])
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- w.prepare(t.Context(), w.session, runner, &out, "task-a", true) }()
	w.reach(booting, done)
	for _, recipe := range []func() *cobra.Command{newCreateCmd, newResumeCmd} {
		w.refusedWithoutEffects(func(orca.Runner) error {
			cmd := recipe()
			cmd.SetArgs([]string{"task-a", "--config", path, "--connection", "server"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			return cmd.ExecuteContext(t.Context())
		}, "another cc-remote command holds the Orca task for task-a")
	}
	close(booting.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	w.prepared(out.Bytes())
	w.free("task-a")
}
