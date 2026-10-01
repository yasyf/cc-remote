package namespace

import (
	"errors"
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
	p := New(Config{
		CLI:               DefaultCLI,
		SSHDir:            DefaultSSHDir(dir),
		StateDir:          filepath.Join(dir, "state"),
		Platform:          "linux/amd64",
		Image:             "cc-remote-linux",
		VolumeSizeGB:      125,
		IdleTimeout:       30 * time.Minute,
		Sizes:             map[string]string{"agents": "l", "stack": "xl"},
		HourlyUSD:         map[string]float64{"l": 0.96, "xl": 1.92},
		StorageGBMonthUSD: 0.2,
		CallTimeout:       time.Minute,
		ReadyTimeout:      time.Minute,
	})
	fake := newFakeNamespace(t, DefaultCLI, p.SSHDir)
	p.Runner = fake
	return p, fake
}

func spec(name string, labels map[string]string) providers.Spec {
	return providers.Spec{Name: name, Profile: "agents", Labels: labels}
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
		site string
		want []string
	}{
		{
			"lean default",
			spec("alpha", nil),
			"",
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "l", "--image", "cc-remote-linux", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent"},
		},
		{
			"full stack opt-in",
			providers.Spec{Name: "alpha", Profile: "stack"},
			"",
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "xl", "--image", "cc-remote-linux", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent"},
		},
		{
			"explicit size, image reference, and region",
			providers.Spec{Name: "alpha", Profile: "agents", Size: "m", Image: "nscr.io/tenant/custom@sha256:abc", Region: "eu-west"},
			"us-east",
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "m", "--image_ref", "nscr.io/tenant/custom@sha256:abc", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent", "--site", "eu-west"},
		},
		{
			"configured site",
			spec("alpha", nil),
			"us-east",
			[]string{"create", "--name", "alpha", "--platform", "linux/amd64", "--size", "l", "--image", "cc-remote-linux", "--volume_size_gb", "125", "--auto_stop_idle_timeout", "30m0s", "--no_checkout", "--persistent", "--site", "us-east"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			p.Site = tt.site
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

func TestRate(t *testing.T) {
	p, _ := newProvider(t)
	tests := []struct {
		name    string
		spec    providers.Spec
		want    providers.Rate
		wantErr string
	}{
		{"agents profile", spec("a", nil), providers.Rate{HourlyUSD: 0.96, StorageGB: 125, StorageGBMonthUSD: 0.2}, ""},
		{"stack profile", providers.Spec{Profile: "stack"}, providers.Rate{HourlyUSD: 1.92, StorageGB: 125, StorageGBMonthUSD: 0.2}, ""},
		{"explicit size", providers.Spec{Profile: "agents", Size: "xl"}, providers.Rate{HourlyUSD: 1.92, StorageGB: 125, StorageGBMonthUSD: 0.2}, ""},
		{"unknown profile", providers.Spec{Profile: "gpu"}, providers.Rate{}, `namespace has no size for profile "gpu" and the spec names none`},
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
