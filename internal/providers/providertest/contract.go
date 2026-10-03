package providertest

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

type Harness struct {
	Provider    providers.Provider
	Spec        func(name string, labels map[string]string) providers.Spec
	TracksState bool
}

func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	tests := []struct {
		name string
		run  func(t *testing.T, h Harness)
	}{
		{"CreateThenGet", createThenGet},
		{"CreateTwice", createTwice},
		{"MissingMachine", missingMachine},
		{"ListFiltersByLabels", listFiltersByLabels},
		{"ExecReportsExitAndStreams", execReportsExitAndStreams},
		{"ExecKeepsArgumentVector", execKeepsArgumentVector},
		{"WakeAndSuspend", wakeAndSuspend},
		{"Access", access},
		{"Destroy", destroy},
		{"ValidateSpec", validateSpec},
		{"Traits", traits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newHarness(t)) })
	}
}

func create(t *testing.T, h Harness, name string, labels map[string]string) providers.Machine {
	t.Helper()
	machine, err := h.Provider.Create(t.Context(), h.Spec(name, labels))
	if err != nil {
		t.Fatalf("Create(%s) = %v", name, err)
	}
	return machine
}

func createThenGet(t *testing.T, h Harness) {
	labels := map[string]string{"cc-remote/workspace": "alpha", "team": "a"}
	created := create(t, h, "alpha", labels)
	got, err := h.Provider.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get(%s) = %v", created.ID, err)
	}
	for _, machine := range []providers.Machine{created, got} {
		if machine.ID != created.ID || machine.ID == "" || machine.Provider == "" || machine.CreatedAt.IsZero() {
			t.Errorf("machine = %+v, want the created ID %q, a provider, and a creation time", machine, created.ID)
		}
		if !maps.Equal(machine.Labels, labels) {
			t.Errorf("labels = %v, want %v", machine.Labels, labels)
		}
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("Get CreatedAt = %v, Create CreatedAt = %v", got.CreatedAt, created.CreatedAt)
	}
}

func createTwice(t *testing.T, h Harness) {
	create(t, h, "alpha", nil)
	if _, err := h.Provider.Create(t.Context(), h.Spec("alpha", nil)); !errors.Is(err, providers.ErrExists) {
		t.Errorf("second Create(alpha) = %v, want ErrExists", err)
	}
}

func missingMachine(t *testing.T, h Harness) {
	ctx := t.Context()
	p := h.Provider
	calls := map[string]func() error{
		"Get":     func() error { _, err := p.Get(ctx, "missing"); return err },
		"Wake":    func() error { return p.Wake(ctx, "missing") },
		"Suspend": func() error { return p.Suspend(ctx, "missing") },
		"Destroy": func() error { return p.Destroy(ctx, "missing") },
		"Exec":    func() error { _, err := p.Exec(ctx, "missing", []string{"true"}, nil); return err },
		"Access":  func() error { _, err := p.Access(ctx, "missing"); return err },
	}
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if err := calls[name](); !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("%s(missing) = %v, want ErrNotFound", name, err)
		}
	}
}

func ids(t *testing.T, p providers.Provider, labels map[string]string) []string {
	t.Helper()
	machines, err := p.List(t.Context(), labels)
	if err != nil {
		t.Fatalf("List(%v) = %v", labels, err)
	}
	listed := make([]string, 0, len(machines))
	for _, machine := range machines {
		listed = append(listed, machine.ID)
	}
	slices.Sort(listed)
	return listed
}

func sorted(ids ...string) []string {
	return slices.Sorted(slices.Values(ids))
}

func listFiltersByLabels(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", map[string]string{"team": "a", "role": "agent"})
	beta := create(t, h, "beta", map[string]string{"team": "b"})
	gamma := create(t, h, "gamma", nil)
	tests := []struct {
		labels map[string]string
		want   []string
	}{
		{map[string]string{"team": "a"}, []string{alpha.ID}},
		{map[string]string{"team": "a", "role": "agent"}, []string{alpha.ID}},
		{map[string]string{"team": "a", "role": "stack"}, []string{}},
		{map[string]string{"team": "c"}, []string{}},
		{nil, sorted(alpha.ID, beta.ID, gamma.ID)},
	}
	for _, tt := range tests {
		if got := ids(t, h.Provider, tt.labels); !slices.Equal(got, tt.want) {
			t.Errorf("List(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func execReportsExitAndStreams(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", nil)
	result, err := h.Provider.Exec(t.Context(), alpha.ID, []string{"sh", "-c", "cat; printf err >&2; exit 3"}, strings.NewReader("in"))
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	if string(result.Stdout) != "in" || string(result.Stderr) != "err" || result.ExitCode != 3 {
		t.Errorf("Exec = %q, %q, exit %d; want \"in\", \"err\", exit 3", result.Stdout, result.Stderr, result.ExitCode)
	}
	result, err = h.Provider.Exec(t.Context(), alpha.ID, []string{"printf", "ok"}, nil)
	if err != nil || string(result.Stdout) != "ok" || result.ExitCode != 0 {
		t.Errorf("Exec without stdin = %q, exit %d, %v; want \"ok\", exit 0", result.Stdout, result.ExitCode, err)
	}
}

func execKeepsArgumentVector(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", nil)
	result, err := h.Provider.Exec(t.Context(), alpha.ID, []string{"printf", "%s|", "a b", "it's", "", "$HOME", "*"}, nil)
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	if want := "a b|it's||$HOME|*|"; string(result.Stdout) != want {
		t.Errorf("Exec stdout = %q, want %q", result.Stdout, want)
	}
}

func wakeAndSuspend(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", nil)
	for _, step := range []struct {
		name string
		run  func(context.Context, string) error
		want providers.State
	}{
		{"Suspend", h.Provider.Suspend, providers.StateSuspended},
		{"Wake", h.Provider.Wake, providers.StateRunning},
	} {
		if err := step.run(t.Context(), alpha.ID); err != nil {
			t.Fatalf("%s(%s) = %v", step.name, alpha.ID, err)
		}
		machine, err := h.Provider.Get(t.Context(), alpha.ID)
		if err != nil {
			t.Fatalf("Get after %s = %v", step.name, err)
		}
		valid := []providers.State{providers.StateRunning, providers.StateSuspended, providers.StateUnknown}
		if h.TracksState {
			valid = []providers.State{step.want}
		}
		if !slices.Contains(valid, machine.State) {
			t.Errorf("state after %s = %q, want one of %v", step.name, machine.State, valid)
		}
	}
}

func access(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", nil)
	got, err := h.Provider.Access(t.Context(), alpha.ID)
	if err != nil {
		t.Fatalf("Access = %v", err)
	}
	switch got.Kind {
	case providers.AccessOpenSSH:
		openSSH(t, got.SSH)
	case providers.AccessCompute:
		if got.Compute.InstanceID != alpha.ID || got.Compute.Container == "" || got.Compute.Endpoint == "" || got.SSH != (providers.Target{}) {
			t.Errorf("compute access = %+v, want the instance %s and its container and endpoint, and no ssh target", got, alpha.ID)
		}
	default:
		t.Errorf("access kind = %q", got.Kind)
	}
}

func openSSH(t *testing.T, target providers.Target) {
	t.Helper()
	if target.Host == "" || target.Port <= 0 || target.User == "" {
		t.Errorf("target = %+v, want a host, a port, and a user", target)
	}
	switch policy := target.HostKeyPolicy; policy.Mode {
	case providers.HostKeyPinned:
		if policy.Alias == "" || policy.KnownHostsFile == "" {
			t.Errorf("pinned policy = %+v, want an alias and a known_hosts file", policy)
		}
	case providers.HostKeyProxyTrusted:
		if target.ProxyCommand == "" {
			t.Errorf("target = %+v trusts a proxy it does not name", target)
		}
	default:
		t.Errorf("host key mode = %q", policy.Mode)
	}
	parsed := OpenSSHConfig(t, target)
	if parsed["hostname"] != target.Host || parsed["user"] != target.User {
		t.Errorf("ssh -G read hostname %q and user %q, want %q and %q", parsed["hostname"], parsed["user"], target.Host, target.User)
	}
}

func OpenSSHConfig(t testing.TB, target providers.Target) map[string]string {
	t.Helper()
	const alias = "cc-remote-target"
	config := "Host " + alias + "\n  " + strings.Join(target.SSHOptions(), "\n  ") + "\n"
	path := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh", "-G", "-F", path, alias).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G rejected\n%s\n%v: %s", config, err, out)
	}
	parsed := map[string]string{}
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		parsed[key] = value
	}
	return parsed
}

func destroy(t *testing.T, h Harness) {
	alpha := create(t, h, "alpha", map[string]string{"team": "a"})
	beta := create(t, h, "beta", nil)
	if err := h.Provider.Destroy(t.Context(), alpha.ID); err != nil {
		t.Fatalf("Destroy(%s) = %v", alpha.ID, err)
	}
	if _, err := h.Provider.Get(t.Context(), alpha.ID); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Get after Destroy = %v, want ErrNotFound", err)
	}
	if got := ids(t, h.Provider, nil); !slices.Equal(got, []string{beta.ID}) {
		t.Errorf("List after Destroy = %v, want [%s]", got, beta.ID)
	}
	if err := h.Provider.Destroy(t.Context(), alpha.ID); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("second Destroy = %v, want ErrNotFound", err)
	}
	recreated := create(t, h, "alpha", nil)
	if len(recreated.Labels) != 0 {
		t.Errorf("recreated alpha carries labels %v from the destroyed one", recreated.Labels)
	}
}

func traits(t *testing.T, h Harness) {
	traits := h.Provider.Traits()
	if !slices.Contains([]providers.TailnetMode{providers.TailnetKernel, providers.TailnetUserspace}, traits.TailnetMode) ||
		!slices.Contains([]providers.Supervisor{providers.SupervisorSpriteEnv, providers.SupervisorSetsid}, traits.Supervisor) {
		t.Errorf("Traits() = %+v, want a known tailnet mode and supervisor", traits)
	}
}

func validateSpec(t *testing.T, h Harness) {
	if err := h.Provider.ValidateSpec(h.Spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
}
