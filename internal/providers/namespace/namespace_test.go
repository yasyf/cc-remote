package namespace

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

func newProvider(t *testing.T) (*Provider, *fakeNamespace) {
	t.Helper()
	dir := t.TempDir()
	p, err := New(Config{
		CLI:               DefaultCLI,
		SSHDir:            DefaultSSHDir(dir),
		StateDir:          filepath.Join(dir, "state"),
		Platform:          "linux/amd64",
		VolumeSizeGB:      125,
		IdleTimeout:       30 * time.Minute,
		HourlyUSD:         map[string]float64{"l": 0.96, "xl": 1.92},
		StorageGBMonthUSD: 0.2,
		CallTimeout:       time.Minute,
		ReadyTimeout:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeNamespace(t, DefaultCLI, p.SSHDir)
	p.Runner = fake
	return p, fake
}

func spec(name string, labels map[string]string) providers.Spec {
	return providers.Spec{Name: name, Profile: "agents", Size: "l", Image: "cc-remote-linux", Labels: labels}
}

func TestNamespaceSatisfiesTheContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Harness {
		p, _ := newProvider(t)
		return providertest.Harness{Provider: p, Spec: spec}
	})
}

func TestCreateArguments(t *testing.T) {
	tests := []struct {
		name string
		spec providers.Spec
		want []string
	}{
		{
			"lean default",
			spec("alpha", nil),
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "l", "--image", "cc-remote-linux", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent"},
		},
		{
			"full stack opt-in",
			providers.Spec{Name: "alpha", Profile: "stack", Size: "xl", Image: "cc-remote-linux"},
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "xl", "--image", "cc-remote-linux", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent"},
		},
		{
			"image reference and region",
			providers.Spec{Name: "alpha", Profile: "agents", Size: "l", Image: "nscr.io/tenant/custom@sha256:abc", Region: "eu-west"},
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "l", "--image_ref", "nscr.io/tenant/custom@sha256:abc", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent", "--site", "eu-west"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			if _, err := p.Create(t.Context(), tt.spec); err != nil {
				t.Fatal(err)
			}
			if got := fake.call("create"); !slices.Equal(got, tt.want) {
				t.Errorf("devbox create args =\n%q\nwant\n%q", got, tt.want)
			}
			if got := fake.call("configure-ssh"); !slices.Equal(got, []string{"configure-ssh", "alpha"}) {
				t.Errorf("devbox configure-ssh args = %q", got)
			}
		})
	}
}

func TestCreateNeedsAnImage(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), providers.Spec{Name: "alpha", Size: "l"}); err == nil || err.Error() != "a namespace spec needs an image" {
		t.Errorf("Create = %v", err)
	}
	if got := fake.call("create"); got != nil {
		t.Errorf("an imageless spec reached devbox create: %q", got)
	}
}

func TestRate(t *testing.T) {
	p, _ := newProvider(t)
	tests := []struct {
		name    string
		spec    providers.Spec
		want    providers.Rate
		wantErr string
	}{
		{"lean size", spec("a", nil), providers.Rate{HourlyUSD: 0.96, StorageGB: 125, StorageGBMonthUSD: 0.2}, ""},
		{"full stack size", providers.Spec{Profile: "stack", Size: "xl"}, providers.Rate{HourlyUSD: 1.92, StorageGB: 125, StorageGBMonthUSD: 0.2}, ""},
		{"no size", providers.Spec{Profile: "agents"}, providers.Rate{}, "a namespace spec needs a size"},
		{"unpriced size", providers.Spec{Profile: "agents", Size: "s"}, providers.Rate{}, `namespace has no hourly rate for size "s"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.Rate(tt.spec)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Rate error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Rate = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeNamespace)
		want  string
	}{
		{"logged in", func(*fakeNamespace) {}, ""},
		{"logged out", func(f *fakeNamespace) { f.loggedIn = false }, "not logged in to Namespace; run 'devbox login'"},
		{"no CLI", func(f *fakeNamespace) { f.cli = "elsewhere" }, `devbox CLI "devbox" not found; install it from https://namespace.so/docs/devbox`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			tt.setup(fake)
			err := p.Check(t.Context())
			if tt.want == "" {
				if err != nil {
					t.Errorf("Check = %v", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSSHTargetTrustsTheDevboxProxy(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	target, err := p.SSHTarget(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := providers.Target{
		Host:          "alpha.devbox.namespace",
		Port:          22,
		User:          "dev",
		IdentityFile:  filepath.Join(p.SSHDir, "alpha.devbox.namespace.key"),
		ProxyCommand:  fake.proxy() + " ssh-proxy alpha",
		HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyProxyTrusted},
	}
	if target != want {
		t.Errorf("SSHTarget =\n%+v\nwant\n%+v", target, want)
	}
}

func TestSSHTargetWithoutAnIdentityFile(t *testing.T) {
	p, fake := newProvider(t)
	fake.noIdentity = true
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	target, err := p.SSHTarget(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if target.IdentityFile != "" {
		t.Errorf("IdentityFile = %q, want none", target.IdentityFile)
	}
	if parsed := providertest.OpenSSHConfig(t, target); parsed["proxycommand"] != target.ProxyCommand {
		t.Errorf("ssh -G read proxycommand %q, want %q", parsed["proxycommand"], target.ProxyCommand)
	}
	if _, err := p.Exec(t.Context(), "alpha", []string{"true"}, nil); err != nil {
		t.Errorf("Exec without an identity file = %v", err)
	}
}

func TestSuspendShutsTheDevboxDown(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	if err := p.Suspend(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if !fake.boxes["alpha"].stopped {
		t.Error("Suspend left the devbox running")
	}
	if err := p.Wake(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if fake.boxes["alpha"].stopped {
		t.Error("Wake left the devbox stopped")
	}
}

func TestWakeStopsOnAPermanentFailure(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.sshFails = providers.Result{
		Stderr:   []byte("Failed: rpc error: code = FailedPrecondition desc = instance is unusable after its last shutdown\n"),
		ExitCode: 255,
	}
	started := time.Now()
	err := p.Wake(t.Context(), "alpha")
	if err == nil || !strings.Contains(err.Error(), "devbox alpha cannot start: Failed: rpc error: code = FailedPrecondition") {
		t.Errorf("Wake = %v, want a permanent failure", err)
	}
	if fake.sshRun != 1 || time.Since(started) >= readyPoll {
		t.Errorf("Wake probed %d times over %s, want one probe", fake.sshRun, time.Since(started))
	}
}

func TestWakeGivesUpAtTheReadyTimeout(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	p.ReadyTimeout = 50 * time.Millisecond
	fake.sshFails = providers.Result{Stderr: []byte("ssh: connect to host alpha.devbox.namespace: Connection refused\n"), ExitCode: 255}
	err := p.Wake(t.Context(), "alpha")
	if err == nil || err.Error() != "devbox alpha not ready after 50ms: ssh: connect to host alpha.devbox.namespace: Connection refused" {
		t.Errorf("Wake = %v", err)
	}
}

func TestExecReportsAnSSHFailure(t *testing.T) {
	p, fake := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.sshFails = providers.Result{Stderr: []byte("kex_exchange_identification: Connection closed by remote host\n"), ExitCode: 255}
	_, err := p.Exec(t.Context(), "alpha", []string{"true"}, nil)
	var failed *providers.CommandError
	if !errors.As(err, &failed) || failed.Result.ExitCode != 255 {
		t.Errorf("Exec = %v, want the ssh failure", err)
	}
}

func TestListReadsAnEmptyAccount(t *testing.T) {
	p, _ := newProvider(t)
	machines, err := p.List(t.Context(), nil)
	if err != nil || len(machines) != 0 {
		t.Errorf("List = %v, %v; want none", machines, err)
	}
}

func TestSSHConfigReadsLiteralValues(t *testing.T) {
	raw := []byte("Host a.devbox.namespace\n" +
		"  IdentityFile \"/Users/x/My Keys/a.key\"\n" +
		"  ProxyCommand /opt/100%%/devbox-ssh-proxy ssh-proxy a\n" +
		"  user dev\n")
	want := map[string]string{
		"host":         "a.devbox.namespace",
		"identityfile": "/Users/x/My Keys/a.key",
		"proxycommand": "/opt/100%/devbox-ssh-proxy ssh-proxy a",
		"user":         "dev",
	}
	if got := sshConfig(raw); !maps.Equal(got, want) {
		t.Errorf("sshConfig =\n%q\nwant\n%q", got, want)
	}
}

func TestCreateLosingARaceReportsErrExists(t *testing.T) {
	p, fake := newProvider(t)
	fake.takenAtCreate = true
	_, err := p.Create(t.Context(), spec("alpha", nil))
	if !errors.Is(err, providers.ErrExists) {
		t.Errorf("Create = %v, want ErrExists", err)
	}
}

func TestCreateThatAllocatesThenFailsIsNotAConflict(t *testing.T) {
	for name, fail := range map[string]func(*fakeNamespace){
		"create":        func(f *fakeNamespace) { f.createFails = true },
		"configure-ssh": func(f *fakeNamespace) { f.sshConfigFail = true },
	} {
		t.Run(name, func(t *testing.T) {
			p, fake := newProvider(t)
			fail(fake)
			_, err := p.Create(t.Context(), spec("alpha", map[string]string{"owner": "me"}))
			if !errors.Is(err, providers.ErrAmbiguous) || errors.Is(err, providers.ErrExists) || !strings.Contains(err.Error(), "rpc error") {
				t.Fatalf("Create = %v, want ErrAmbiguous carrying the CLI failure, never ErrExists", err)
			}
			machine, err := p.Get(t.Context(), "alpha")
			if err != nil || len(machine.Labels) != 0 {
				t.Errorf("after the failed create, Get = %+v, %v; want the allocated devbox without labels", machine, err)
			}
		})
	}
}
