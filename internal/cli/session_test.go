package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

func TestTheExampleConfigOpensEveryShippedProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]providers.Traits{
		"sprites":   {TailnetMode: providers.TailnetKernel, Supervisor: providers.SupervisorSpriteEnv, HostKeys: true},
		"namespace": {TailnetMode: providers.TailnetUserspace, Supervisor: providers.SupervisorSetsid, CredentialHelper: "/.namespace/devbox/git-credential-nsc"},
	} {
		provider, err := openProvider(cfg, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if provider.Traits() != want {
			t.Errorf("%s traits = %+v", kind, provider.Traits())
		}
		profile := cfg.Profiles[cfg.Profile].Machine[kind]
		if err := provider.ValidateSpec(providers.Spec{Name: "workspace", Profile: cfg.Profile, Image: profile.Image, Size: profile.Size}); err != nil && kind == "sprites" {
			t.Errorf("%s spec: %v", kind, err)
		}
	}
	if _, err := openProvider(cfg, "other"); err == nil {
		t.Error("an unknown provider opened")
	}
}

func TestPlatformFollowsTheProviderTraits(t *testing.T) {
	got := platform(providers.Traits{TailnetMode: providers.TailnetUserspace, Supervisor: providers.SupervisorSetsid, CredentialHelper: "/h"})
	if got.Daemon != (tailnet.Daemon{Mode: tailnet.Userspace, Supervisor: tailnet.Setsid}) || got.HostKeys || got.CredentialHelper != "/h" {
		t.Errorf("platform = %+v", got)
	}
}

func TestEveryLifecycleCommandTakesTheSelectionFlags(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"create", "resume", "suspend", "destroy", "prepare", "drain", "status", "verify"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("no %s command: %v", name, err)
		}
		for _, flag := range []string{"config", "provider", "profile"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s lacks --%s", name, flag)
			}
		}
	}
	for _, name := range []string{"create", "resume", "suspend", "destroy"} {
		cmd, _, _ := root.Find([]string{name})
		if cmd.Flags().Lookup("connection") == nil {
			t.Errorf("%s lacks --connection, which Orca's recipes pass", name)
		}
	}
	if cmd, _, _ := root.Find([]string{"warm"}); cmd.Name() != "prepare" {
		t.Error("warm is not an alias of prepare")
	}
	if cmd, _, _ := root.Find([]string{"proxy"}); !cmd.Hidden {
		t.Error("proxy is not hidden")
	}
}

func TestVerifyFailsWhenAConfigIsMissing(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"verify", "--config", filepath.Join(t.TempDir(), "none.yaml")})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %v", err)
	}
}

func TestEmitWritesIndentedJSON(t *testing.T) {
	var out bytes.Buffer
	if err := emit(&out, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]int
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded["a"] != 1 || !strings.Contains(out.String(), "\n  ") {
		t.Errorf("emit wrote %q, %v", out.String(), err)
	}
}

func TestLifecycleCommandsRejectIncompleteRequests(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unsupported connection", []string{"create", "ws-1", "--connection", "server"}, "not implemented"},
		{"destroy without name or payload", []string{"destroy", "--connection", "ssh"}, "stdin"},
		{"create without name", []string{"create"}, "name the workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := NewRootCmd()
			root.SetIn(strings.NewReader(""))
			root.SetArgs(tt.args)
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
