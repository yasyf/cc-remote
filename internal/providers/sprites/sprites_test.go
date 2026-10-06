package sprites

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "proxy" && os.Args[2] == "--" {
		if err := Proxy(context.Background(), os.Args[3:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newProvider(t *testing.T) (*Provider, *fakeSprites) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "sprite")
	script := "#!/bin/sh\necho $$ > " + filepath.Join(dir, "proxy.pid") + "\ntrap '' HUP\nexec sleep 60\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{
		Org:      "acme",
		CLI:      cli,
		StateDir: filepath.Join(dir, "state"),
		Helper:   helper,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeSprites(t, cli, "acme")
	p.Runner = fake
	return p, fake
}

func spec(name string, labels map[string]string) providers.Spec {
	return providers.Spec{Name: name, Profile: "agents", Labels: labels}
}

func TestSpritesSatisfiesTheContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Harness {
		p, _ := newProvider(t)
		return providertest.Harness{Provider: p, Spec: spec}
	})
}

func TestExecChecksTheAPIOnlyAfterANonzeroCommandResult(t *testing.T) {
	tests := []struct {
		name, id, script, message string
		status                    int
		cancel                    bool
		result                    providers.Result
		is                        error
		verbs                     []string
	}{
		{name: "success ignores a failing metadata API", id: "s1", script: "printf out; printf err >&2", status: 503, result: providers.Result{Stdout: []byte("out"), Stderr: []byte("err")}, verbs: []string{"exec"}},
		{name: "remote failure keeps its streams and exit", id: "s1", script: "printf out; printf err >&2; exit 3", result: providers.Result{Stdout: []byte("out"), Stderr: []byte("err"), ExitCode: 3}, verbs: []string{"exec", "api"}},
		{name: "missing machine retains the not-found contract", id: "gone", script: "true", result: providers.Result{Stderr: []byte("sprite not found"), ExitCode: 1}, is: providers.ErrNotFound, verbs: []string{"exec", "api"}},
		{name: "metadata failure remains visible after a failed command", id: "s1", script: "printf out; exit 3", status: 503, result: providers.Result{Stdout: []byte("out"), ExitCode: 3}, message: `sprites api answered 503 for s1: {"error":"unauthorized"}`, verbs: []string{"exec", "api"}},
		{name: "invalid name never reaches the CLI", id: "Invalid", script: "true", is: providers.ErrNotFound},
		{name: "cancelled execution makes no metadata request", id: "s1", script: "true", cancel: true, is: context.Canceled, verbs: []string{"exec"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			if _, err := p.Create(t.Context(), spec("s1", nil)); err != nil {
				t.Fatal(err)
			}
			before := len(fake.verbs())
			fake.status = tt.status
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			result, err := p.Exec(ctx, tt.id, []string{"sh", "-c", tt.script}, nil)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) {
					t.Errorf("Exec error = %v, want %v", err, tt.is)
				}
			case tt.message != "":
				if err == nil || err.Error() != tt.message {
					t.Errorf("Exec error = %v, want %q", err, tt.message)
				}
			case err != nil:
				t.Errorf("Exec error = %v", err)
			}
			if string(result.Stdout) != string(tt.result.Stdout) || string(result.Stderr) != string(tt.result.Stderr) || result.ExitCode != tt.result.ExitCode {
				t.Errorf("Exec result = %+v, want %+v", result, tt.result)
			}
			if got := fake.verbs()[before:]; !slices.Equal(got, tt.verbs) {
				t.Errorf("CLI calls = %v, want %v", got, tt.verbs)
			}
		})
	}
}

func TestSpecAdmission(t *testing.T) {
	p, _ := newProvider(t)
	tests := []struct {
		name string
		spec providers.Spec
		want string
	}{
		{"lean", spec("alpha", nil), ""},
		{"image", providers.Spec{Name: "alpha", Profile: "agents", Image: "ubuntu"}, `a sprite boots no image; provision alpha after create instead of naming "ubuntu"`},
		{"size", providers.Spec{Name: "alpha", Profile: "agents", Size: "xl"}, `sprites have one size, not "xl"`},
		{"region", providers.Spec{Name: "alpha", Profile: "agents", Region: "ord"}, `sprites choose no region, not "ord"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.ValidateSpec(tt.spec)
			if tt.want == "" {
				if err != nil {
					t.Errorf("ValidateSpec = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Errorf("ValidateSpec error = %v, want %q", err, tt.want)
			}
			if _, err := p.Create(t.Context(), tt.spec); err == nil || err.Error() != tt.want {
				t.Errorf("Create error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCreateRejectsAnInvalidName(t *testing.T) {
	p, fake := newProvider(t)
	for _, name := range []string{"", "Alpha", "a_b", "-a", strings.Repeat("a", nameLimit+1)} {
		if _, err := p.Create(t.Context(), spec(name, nil)); err == nil {
			t.Errorf("Create(%q) succeeded", name)
		}
	}
	if verbs := fake.verbs(); len(verbs) != 0 {
		t.Errorf("invalid names reached the CLI: %v", verbs)
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Provider, *fakeSprites)
		want  string
	}{
		{"logged in", func(*Provider, *fakeSprites) {}, ""},
		{"logged out", func(_ *Provider, f *fakeSprites) { f.status = 401 }, "not logged in to sprites org acme; run 'sprite login -o acme'"},
		{"no CLI", func(_ *Provider, f *fakeSprites) { f.cli = "elsewhere" }, "not found; install it from https://sprites.dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			tt.setup(p, fake)
			err := p.Check(t.Context())
			if tt.want == "" {
				if err != nil {
					t.Errorf("Check = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestListFollowsContinuationTokens(t *testing.T) {
	p, fake := newProvider(t)
	names := []string{"a1", "a2", "a3", "a4", "a5"}
	for _, name := range names {
		if _, err := p.Create(t.Context(), spec(name, map[string]string{"pool": "x"})); err != nil {
			t.Fatal(err)
		}
	}
	before := len(fake.verbs())
	machines, err := p.List(t.Context(), map[string]string{"pool": "x"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(machines))
	for _, machine := range machines {
		got = append(got, machine.ID)
	}
	if !slices.Equal(got, names) {
		t.Errorf("List = %v, want %v", got, names)
	}
	if pages := len(fake.verbs()) - before; pages != 3 {
		t.Errorf("List read %d pages, want 3", pages)
	}
}

func TestStatusMapsToState(t *testing.T) {
	tests := []struct {
		status string
		want   providers.State
	}{
		{"running", providers.StateRunning},
		{"warm", providers.StateSuspended},
		{"cold", providers.StateSuspended},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			p, fake := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			fake.sprites["alpha"].status = tt.status
			machine, err := p.Get(t.Context(), "alpha")
			if err != nil || machine.State != tt.want {
				t.Errorf("Get = %q, %v; want %q", machine.State, err, tt.want)
			}
		})
	}
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.sprites["alpha"].status = "creating"
	if _, err := p.Get(t.Context(), "alpha"); err == nil || err.Error() != `sprite alpha has status "creating"` {
		t.Errorf("Get with an unknown status = %v", err)
	}
}

func TestAccessPinsTheSpriteHostKey(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	access, err := p.Access(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if access.Kind != providers.AccessOpenSSH || access.Compute != (providers.ComputeInstance{}) {
		t.Errorf("Access = %+v, want openssh access only", access)
	}
	target := access.SSH
	keys := p.keys()
	want := providers.Target{
		Host:         "alpha",
		Port:         22,
		User:         "sprite",
		IdentityFile: keys.identity("alpha"),
		ProxyCommand: providers.ShellQuote(p.helperPath(), "proxy", "--", p.CLI, "proxy", "-o", "acme", "-s", "alpha", "-W", ":22"),
		HostKeyPolicy: providers.HostKeyPolicy{
			Mode:           providers.HostKeyPinned,
			Alias:          "alpha.sprite.cc-remote",
			KnownHostsFile: keys.knownHosts("alpha"),
		},
	}
	if target != want {
		t.Errorf("Access target =\n%+v\nwant\n%+v", target, want)
	}
	knownHosts, err := os.ReadFile(keys.knownHosts("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha.sprite.cc-remote ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeSpriteHostKey\n"; string(knownHosts) != want {
		t.Errorf("known_hosts = %q, want %q", knownHosts, want)
	}
	public, err := os.ReadFile(keys.identity("alpha") + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if authorized := fake.sprites["alpha"].authorized; !slices.Equal(authorized, []string{strings.TrimSpace(string(public))}) {
		t.Errorf("authorized keys = %q, want the generated public key", authorized)
	}
	if _, err := p.Access(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(keys.identity("alpha") + ".pub")
	if err != nil || string(again) != string(public) {
		t.Errorf("a second Access replaced the key: %v", err)
	}

	if err := p.Destroy(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{keys.identity("alpha"), keys.identity("alpha") + ".pub", keys.knownHosts("alpha")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Destroy left %s: %v", path, err)
		}
	}
}

func TestConcurrentAccessesShareOneKey(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, errs[i] = p.Access(t.Context(), "alpha") })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(p.keys().identity("alpha") + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range fake.sprites["alpha"].authorized {
		if key != strings.TrimSpace(string(public)) {
			t.Errorf("authorized %q, want only the one key on disk", key)
		}
	}
}

func TestDestroyKeepsTheReceiptUntilTheSpriteIsGone(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", map[string]string{"owner": "me"})); err != nil {
		t.Fatal(err)
	}
	fake.destroySurvives = true
	if err := p.Destroy(t.Context(), "alpha"); err != nil {
		t.Fatalf("Destroy = %v", err)
	}
	if machine, err := p.Get(t.Context(), "alpha"); err != nil || machine.Labels["owner"] != "me" {
		t.Fatalf("after the destroy the sprite survived, Get = %+v, %v; want its labels kept", machine, err)
	}
	fake.destroySurvives = false
	if err := p.Destroy(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(t.Context(), "alpha"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("after the sprite went, Get = %v", err)
	}
}

func TestSuspendFailsAndLeavesTheSpriteAsItIs(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	before := len(fake.verbs())
	err := p.Suspend(t.Context(), "alpha")
	want := "suspending sprite alpha: unsupported operation: Sprites has no stop verb, so cc-remote cannot stop a sprite; it sleeps on its own only once no session, request, or connection holds it, and an open cc-remote orca forward holds it active; run cc-remote destroy to free it"
	if !errors.Is(err, errors.ErrUnsupported) || err.Error() != want {
		t.Errorf("Suspend = %v, want %q", err, want)
	}
	if verbs := fake.verbs()[before:]; !slices.Equal(verbs, []string{"api"}) {
		t.Errorf("Suspend ran sprite %q, want only the api read", verbs)
	}
	if machine, err := p.Get(t.Context(), "alpha"); err != nil || machine.State != providers.StateRunning {
		t.Errorf("after the failed suspend, Get = %+v, %v; want the sprite running", machine, err)
	}
}

func TestCreateLosingARaceIsAmbiguous(t *testing.T) {
	p, fake := newProvider(t)
	fake.takenAtCreate = true
	_, err := p.Create(t.Context(), spec("alpha", nil))
	if !errors.Is(err, providers.ErrAmbiguous) || errors.Is(err, providers.ErrExists) || !strings.Contains(err.Error(), "sprite alpha already exists") {
		t.Errorf("Create = %v, want ErrAmbiguous carrying the CLI conflict, never ErrExists", err)
	}
}

func TestCreateThatAllocatesThenFailsIsNotAConflict(t *testing.T) {
	for name, stderr := range map[string]string{
		"a timeout":             "error: waiting for the sprite console timed out",
		"the requested sprite":  "error: sprite alpha already exists",
		"another sprite":        "error: sprite alpha-2 already exists",
		"an unrelated resource": "error: creating the console socket: file already exists",
		"the name elsewhere":    "creating sprite alpha\nerror: volume already exists",
		"the name on the line":  "error: creating sprite alpha: volume already exists",
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := newProvider(t)
			fake.createFails = stderr
			_, err := p.Create(t.Context(), spec("alpha", map[string]string{"owner": "me"}))
			if !errors.Is(err, providers.ErrAmbiguous) || errors.Is(err, providers.ErrExists) || !strings.Contains(err.Error(), strings.Split(stderr, "\n")[0]) {
				t.Fatalf("Create = %v, want ErrAmbiguous carrying the CLI failure, never ErrExists", err)
			}
			machine, err := p.Get(t.Context(), "alpha")
			if err != nil || len(machine.Labels) != 0 {
				t.Errorf("after the failed create, Get = %+v, %v; want the allocated sprite without labels", machine, err)
			}
		})
	}
}

func TestCreateNamesTheStepThatFailed(t *testing.T) {
	tests := []struct {
		name      string
		spec      providers.Spec
		edit      func(*fakeSprites)
		step      CreateStep
		status    int
		exit      int
		ambiguous bool
		verbs     []string
	}{
		{"an invalid name", spec("Alpha", nil), func(*fakeSprites) {}, StepValidate, 0, 0, false, nil},
		{"an unsupported spec", providers.Spec{Name: "alpha", Profile: "agents", Region: "ord"}, func(*fakeSprites) {}, StepValidate, 0, 0, false, nil},
		{"a name already taken", spec("alpha", nil), func(f *fakeSprites) { f.sprites["alpha"] = &fakeSprite{status: "cold"} }, StepPreflight, 0, 0, false, []string{"api"}},
		{"a lookup the API refuses", spec("alpha", nil), func(f *fakeSprites) { f.status = 503 }, StepPreflight, 503, 0, false, []string{"api"}},
		{"a create the CLI rejects", spec("alpha", nil), func(f *fakeSprites) { f.createRejects = "error: creating sprite alpha: rejected" }, StepCommand, 0, 1, false, []string{"api", "create", "api"}},
		{"a create that allocates then fails", spec("alpha", nil), func(f *fakeSprites) { f.createFails = "error: waiting for the sprite console timed out" }, StepCommand, 0, 1, true, []string{"api", "create", "api"}},
		{"a create that loses a race", spec("alpha", nil), func(f *fakeSprites) { f.takenAtCreate = true }, StepCommand, 0, 1, true, []string{"api", "create", "api"}},
		{"a readback the API refuses", spec("alpha", nil), func(f *fakeSprites) { f.readbackStatus = 500 }, StepReadback, 500, 0, true, []string{"api", "create", "api"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			tt.edit(fake)
			_, err := p.Create(t.Context(), tt.spec)
			var failed *CreateError
			if !errors.As(err, &failed) || failed.Step != tt.step {
				t.Fatalf("Create = %v, want a failure at %s", err, tt.step)
			}
			status, exit := 0, 0
			var answered *StatusError
			if errors.As(err, &answered) {
				status = answered.Status
			}
			var command *providers.CommandError
			if errors.As(err, &command) {
				exit = command.Result.ExitCode
			}
			if status != tt.status || exit != tt.exit || errors.Is(err, providers.ErrAmbiguous) != tt.ambiguous {
				t.Errorf("Create = %v with status %d and exit %d, want status %d, exit %d, and ambiguous %t", err, status, exit, tt.status, tt.exit, tt.ambiguous)
			}
			if verbs := fake.verbs(); !slices.Equal(verbs, tt.verbs) {
				t.Errorf("CLI verbs = %q, want %q with no retry or destroy", verbs, tt.verbs)
			}
		})
	}
}

func TestProxyCommandStopsWithSSH(t *testing.T) {
	for exit, end := range map[string]func(*exec.Cmd, io.Closer) error{
		"closes the connection": func(_ *exec.Cmd, stdin io.Closer) error { return stdin.Close() },
		"hangs up":              func(ssh *exec.Cmd, _ io.Closer) error { return ssh.Process.Signal(syscall.SIGHUP) },
	} {
		t.Run(exit, func(t *testing.T) {
			p, _ := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			access, err := p.Access(t.Context(), "alpha")
			if err != nil {
				t.Fatal(err)
			}
			ssh := exec.Command("sh", "-c", "exec "+access.SSH.ProxyCommand)
			stdin, err := ssh.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := ssh.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := ssh.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if ssh.ProcessState == nil {
					_ = ssh.Process.Kill()
					_ = ssh.Wait()
				}
			})
			pid := readPID(t, filepath.Join(filepath.Dir(p.CLI), "proxy.pid"))
			if err := end(ssh, stdin); err != nil {
				t.Fatal(err)
			}
			released := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, stderr)
				close(released)
			}()
			select {
			case <-released:
			case <-time.After(10 * time.Second):
				t.Fatalf("when ssh %s, the sprite proxy still holds ssh's stderr", exit)
			}
			_ = ssh.Wait()
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("when ssh %s, sprite proxy %d outlives it: %v", exit, pid, err)
			}
		})
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := os.ReadFile(path)
		if pid, parsed := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && parsed == nil {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sprite proxy never started: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelperOutlivesTheBinaryThatInstalledIt(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "cache", "cc-remote")
	p := &Provider{Config: Config{Helper: source, StateDir: filepath.Join(dir, "state")}}
	install := func(content string) os.FileInfo {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := p.installHelper(); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Dir(source)); err != nil {
			t.Fatal(err)
		}
		out, err := os.ReadFile(p.helperPath())
		if err != nil || string(out) != content {
			t.Fatalf("installed helper = %q, %v; want %q", out, err, content)
		}
		info, err := os.Stat(p.helperPath())
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("helper mode = %v, want 0700", info.Mode().Perm())
		}
		return info
	}
	first := install("v1")
	if !os.SameFile(first, install("v1")) {
		t.Error("an unchanged helper was rewritten")
	}
	install("v2")
}
