package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

var payloadBuildName = regexp.MustCompile(`^cc-remote-payload-[0-9a-f]{8}$`)

type buildMachine struct {
	failing string
	steps   []string
	stdins  map[string]string
}

func (m *buildMachine) Handle(_ string, cmd []string, stdin []byte) providers.Result {
	var step string
	switch {
	case slices.Equal(cmd[:3], []string{"sudo", "bash", "-s"}):
		step = "provision " + strings.Join(cmd[3:], " ")
	case cmd[0] == "sh":
		step = "stage plugins"
	default:
		step = "plugins " + strings.Join(cmd[slices.Index(cmd, "plugins.sh")+1:], " ")
	}
	m.steps = append(m.steps, step)
	if m.stdins == nil {
		m.stdins = map[string]string{}
	}
	m.stdins[step] = string(stdin)
	if step == m.failing {
		return providers.Result{Stderr: []byte("boom\n"), ExitCode: 1}
	}
	return providers.Result{Stderr: []byte(step + "\n")}
}

type downloading struct {
	*providertest.Fake
	payload   string
	fail      error
	createErr error
	leaves    func(name string) map[string]string
	created   providers.Spec
	reads     []string
}

func (d *downloading) Create(ctx context.Context, spec providers.Spec) (providers.Machine, error) {
	d.created = spec
	if d.createErr == nil {
		return d.Fake.Create(ctx, spec)
	}
	if d.leaves != nil {
		spec.Labels = d.leaves(spec.Name)
		if _, err := d.Fake.Create(ctx, spec); err != nil {
			return providers.Machine{}, err
		}
	}
	return providers.Machine{}, d.createErr
}

func (d *downloading) Download(ctx context.Context, id, path string, w io.Writer) error {
	if _, err := d.Get(ctx, id); err != nil {
		return err
	}
	d.reads = append(d.reads, id+" "+path)
	if d.fail != nil {
		return d.fail
	}
	_, err := io.WriteString(w, d.payload)
	return err
}

const buildToken = "payload-build-token"

func payloadConfig(t *testing.T, image, inventoryText string) *config.Config {
	t.Helper()
	inventoryPath := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(inventoryPath, []byte(inventoryText), 0o600); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`
repository: https://github.com/example/app
ref: main
provider: fake
profile: lean
state_dir: %s
providers:
  fake: {}
workspace_dirs:
  fake: /home/fake
profiles:
  lean:
    prepare: [true]
    machine:
      fake: { image: %q }
inventory: %s
forwards:
  - { label: web, env: WEB_PORT }
`, t.TempDir(), image, inventoryPath)
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	return cfg
}

func fullScripts(t *testing.T, cfg *config.Config) images.Scripts {
	t.Helper()
	inventory, err := images.Load(cfg.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := images.Render(inventory, "lean")
	if err != nil {
		t.Fatal(err)
	}
	return scripts
}

func TestBuildPayloadPacksTheFullInventoryAndRemovesTheMachine(t *testing.T) {
	tests := []struct {
		name      string
		inventory string
		tokens    int
		install   string
	}{
		{name: "a private marketplace takes the token on the install's stdin", inventory: private, tokens: 1, install: buildToken + "\n"},
		{name: "public tools read no token", inventory: inventory, install: "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := payloadConfig(t, "", tt.inventory)
			scripts := fullScripts(t, cfg)
			machine := &buildMachine{}
			provider := &downloading{Fake: &providertest.Fake{Handle: machine.Handle}, payload: "hsqs squashfs payload"}
			tokens := 0
			token := func(context.Context) (string, error) {
				tokens++
				return buildToken, nil
			}
			var out, stderr bytes.Buffer
			build, err := BuildPayload(t.Context(), cfg, provider, "fake", "lean", token, &out, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(provider.payload))
			want := PayloadBuild{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(provider.payload)), Tools: scripts.Fingerprint(), Machine: provider.created.Name}
			if build != want {
				t.Errorf("build = %+v, want %+v", build, want)
			}
			if !payloadBuildName.MatchString(build.Machine) {
				t.Errorf("machine %q does not match %s", build.Machine, payloadBuildName)
			}
			if spec := provider.created; spec.Image != "" || spec.Profile != "lean" || !maps.Equal(spec.Labels, map[string]string{LabelPayloadBuild: build.Machine}) {
				t.Errorf("created %+v", spec)
			}
			if out.String() != provider.payload {
				t.Errorf("wrote %q, want %q", out.String(), provider.payload)
			}
			pack := "provision pack " + scripts.Fingerprint()
			steps := []string{"provision packages", "provision tools", "stage plugins", "plugins install", "plugins natives", "plugins verify", pack}
			if !slices.Equal(machine.steps, steps) {
				t.Errorf("steps = %q, want %q", machine.steps, steps)
			}
			stdins := map[string]string{
				"provision packages": string(scripts.ProvisionScript),
				"provision tools":    string(scripts.ProvisionScript),
				"stage plugins":      string(scripts.Plugins),
				"plugins install":    tt.install,
				"plugins natives":    "",
				"plugins verify":     "",
				pack:                 string(scripts.ProvisionScript),
			}
			if !maps.Equal(machine.stdins, stdins) {
				t.Errorf("stdins = %q, want %q", machine.stdins, stdins)
			}
			if tokens != tt.tokens {
				t.Errorf("read the token %d times, want %d", tokens, tt.tokens)
			}
			calls := provider.Calls()
			if strings.Contains(strings.Join(calls, "\n"), buildToken) || strings.Contains(stderr.String(), buildToken) {
				t.Errorf("the token left the install's stdin: calls %q, stderr %q", calls, stderr.String())
			}
			if got := stderr.String(); got != strings.Join(steps, "\n")+"\n" {
				t.Errorf("stderr = %q", got)
			}
			if reads := []string{build.Machine + " " + images.PackPath}; !slices.Equal(provider.reads, reads) {
				t.Errorf("reads = %q, want %q", provider.reads, reads)
			}
			if calls[0] != "create "+build.Machine || calls[len(calls)-1] != "destroy "+build.Machine {
				t.Errorf("calls = %q", calls)
			}
			if _, err := provider.Get(t.Context(), build.Machine); !errors.Is(err, providers.ErrNotFound) {
				t.Errorf("Get after the build = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestBuildPayloadRemovesTheMachineWhenAStepFails(t *testing.T) {
	failed := errors.New("the read broke")
	tests := []struct {
		name    string
		failing string
		fail    error
		steps   int
		reads   int
		is      error
		message string
	}{
		{name: "a phase fails", failing: "provision tools", steps: 2, message: "exited 1: boom"},
		{name: "the install fails", failing: "plugins install", steps: 4, message: "exited 1: boom"},
		{name: "the natives phase fails", failing: "plugins natives", steps: 5, message: "exited 1: boom"},
		{name: "the full verification fails before the pack", failing: "plugins verify", steps: 6, message: "exited 1: boom"},
		{name: "the download fails", fail: failed, steps: 7, reads: 1, is: failed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := &buildMachine{failing: tt.failing}
			provider := &downloading{Fake: &providertest.Fake{Handle: machine.Handle}, payload: "hsqs", fail: tt.fail}
			token := func(context.Context) (string, error) { return buildToken, nil }
			var out bytes.Buffer
			_, err := BuildPayload(t.Context(), payloadConfig(t, "", private), provider, "fake", "lean", token, &out, io.Discard)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) {
					t.Errorf("BuildPayload = %v, want %v", err, tt.is)
				}
			case err == nil || !strings.HasSuffix(err.Error(), tt.message):
				t.Errorf("BuildPayload = %v, want it to end %q", err, tt.message)
			}
			name := provider.created.Name
			if len(machine.steps) != tt.steps || len(provider.reads) != tt.reads || out.Len() != 0 {
				t.Errorf("steps %q, reads %q, wrote %q", machine.steps, provider.reads, out.String())
			}
			if calls := provider.Calls(); calls[len(calls)-1] != "destroy "+name {
				t.Errorf("calls = %q, want the last to destroy %s", calls, name)
			}
			if _, err := provider.Get(t.Context(), name); !errors.Is(err, providers.ErrNotFound) {
				t.Errorf("Get after the failed build = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestBuildPayloadDiscardsAFailedCreateOnlyWhenItsLabelProvesTheBuildMadeIt(t *testing.T) {
	timedOut := errors.New("the create timed out")
	tests := []struct {
		name      string
		createErr error
		leaves    func(name string) map[string]string
		verbs     string
		kept      bool
		message   string
	}{
		{name: "a create that left a labelled machine destroys it", createErr: timedOut, leaves: func(name string) map[string]string { return map[string]string{LabelPayloadBuild: name} }, verbs: "create destroy"},
		{name: "a create that left an unlabelled machine keeps it", createErr: timedOut, leaves: func(string) map[string]string { return map[string]string{} }, verbs: "create", kept: true, message: "without a label proving this build made it"},
		{name: "a create that left another build's machine keeps it", createErr: timedOut, leaves: func(string) map[string]string {
			return map[string]string{LabelPayloadBuild: "cc-remote-payload-00000000"}
		}, verbs: "create", kept: true, message: "so it was left running"},
		{name: "a create that made nothing destroys nothing", createErr: timedOut, verbs: ""},
		{name: "a taken name keeps the machine", createErr: fmt.Errorf("fake: %w", providers.ErrExists), leaves: func(string) map[string]string { return map[string]string{} }, verbs: "create", kept: true, message: "already exists at the provider, so it was left alone: fake: machine already exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := &buildMachine{}
			provider := &downloading{Fake: &providertest.Fake{Handle: machine.Handle}, createErr: tt.createErr, leaves: tt.leaves}
			token := func(context.Context) (string, error) { return buildToken, nil }
			_, err := BuildPayload(t.Context(), payloadConfig(t, "", inventory), provider, "fake", "lean", token, io.Discard, io.Discard)
			if !errors.Is(err, tt.createErr) || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("BuildPayload = %v, want %v carrying %q", err, tt.createErr, tt.message)
			}
			name := provider.created.Name
			if got := verbs(provider.Calls()); got != tt.verbs {
				t.Errorf("calls = %q, want verbs %q", provider.Calls(), tt.verbs)
			}
			if _, err := provider.Get(t.Context(), name); (err == nil) != tt.kept {
				t.Errorf("Get after the failed create = %v, want kept %v", err, tt.kept)
			}
			if len(machine.steps) != 0 {
				t.Errorf("a failed create ran %q", machine.steps)
			}
		})
	}
}

func TestBuildPayloadRefusesWhatCannotBuildOne(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		download bool
		tokenErr error
		tokens   int
		want     string
	}{
		{name: "an imaged profile", image: "agent-host", download: true, want: `profile lean boots fake machines from image "agent-host"; a payload is built on a machine provisioned in place`},
		{name: "a provider that cannot download", want: "the fake provider cannot download a file from a machine, so it cannot build a payload"},
		{name: "an unreadable token", download: true, tokenErr: errors.New("no GH_TOKEN or GITHUB_TOKEN"), tokens: 1, want: "no GH_TOKEN or GITHUB_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &providertest.Fake{}
			var provider providers.Provider = fake
			if tt.download {
				provider = &downloading{Fake: fake}
			}
			tokens := 0
			token := func(context.Context) (string, error) {
				tokens++
				return "", tt.tokenErr
			}
			_, err := BuildPayload(t.Context(), payloadConfig(t, tt.image, private), provider, "fake", "lean", token, io.Discard, io.Discard)
			if err == nil || err.Error() != tt.want {
				t.Errorf("BuildPayload = %v, want %q", err, tt.want)
			}
			if tokens != tt.tokens {
				t.Errorf("read the token %d times, want %d", tokens, tt.tokens)
			}
			if calls := fake.Calls(); len(calls) != 0 {
				t.Errorf("calls = %q, want none", calls)
			}
		})
	}
}

func TestGitTokenReadsTheEnvironmentBeforeTheCommand(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		command []string
		want    string
		message string
	}{
		{name: "GH_TOKEN wins", env: map[string]string{"GH_TOKEN": "gh", "GITHUB_TOKEN": "github"}, command: []string{"false"}, want: "gh"},
		{name: "GITHUB_TOKEN follows", env: map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": "github"}, command: []string{"false"}, want: "github"},
		{name: "the command answers last", env: map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": ""}, command: []string{"printf", " command \n"}, want: "command"},
		{name: "a failing command", env: map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": ""}, command: []string{"false"}, message: `no GH_TOKEN or GITHUB_TOKEN, and "false" failed: exit status 1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			got, err := GitToken(&config.Config{Git: config.Git{TokenCommand: tt.command}})(t.Context())
			if tt.message != "" {
				if err == nil || err.Error() != tt.message {
					t.Errorf("GitToken = %q, %v, want %q", got, err, tt.message)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("GitToken = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}
