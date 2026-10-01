package images

import (
	"strings"
	"testing"
)

const (
	digest = "1111111111111111111111111111111111111111111111111111111111111111"
	commit = "2222222222222222222222222222222222222222"
)

func validInventory() Inventory {
	return Inventory{
		Version: SchemaVersion,
		Image:   &Image{Name: "agent-host", Base: "ubuntu:24.04@sha256:" + digest, User: "agent", WorkspaceDir: "/workspaces"},
		System: []Artifact{
			{Name: "uv", Version: "0.1.0", URL: "https://example.com/uv.tar.gz", SHA256: digest, Format: TarGz, Bins: map[string]string{"uv": "uv/uv"}},
			{Name: "claude", Version: "2.0.0", URL: "https://example.com/claude", SHA256: digest, Format: Binary},
			{Name: "codex", Version: "0.2.0", URL: "https://example.com/codex.tar.gz", SHA256: digest, Format: TarGz, Bins: map[string]string{"codex": "codex"}},
		},
		Tools: []Artifact{
			{Name: "jq", Version: "1.8.2", URL: "https://example.com/jq", SHA256: digest, Format: Binary},
			{Name: "cookiesync", Version: "0.30.0", URL: "https://example.com/cookiesync.tar.gz", SHA256: digest, Format: TarGz, Bins: map[string]string{"cookiesync": "cookiesync"}},
		},
		Links: []string{"claude"},
		Python: Python{
			Version: "3.13",
			System:  []PythonTool{{Name: "cc-transcript", Version: "1.0.0", Bins: []string{"cc-transcript"}}},
			User:    []PythonTool{{Name: "wlm", Package: "wlm[lab]", Marketplace: "market", Bins: []string{"wlm"}}},
		},
		Claude: Claude{
			Env:          map[string]string{"DOMAIN": "example.com"},
			Marketplaces: []Marketplace{{Name: "market", GitHub: "owner/market", Ref: commit}},
			Plugins:      []Plugin{{ID: "hooks@market", Version: "1.0.0", Bins: []string{"bin/hooks"}}},
		},
		CodexRuntime: &CodexRuntime{Version: "26.1.0", URL: "https://example.com/runtime.tar.xz", SHA256: digest, Plugins: []string{"pdf"}},
		CaptainHook:  &CaptainHook{Version: "1.0.0", Sums: "https://example.com/SHA256SUMS.txt", Asset: "./hook.tar.gz", SHA256: digest, Plugin: "hooks@market"},
		Cookiesync:   &Cookiesync{SchemaFingerprint: digest},
		Services: []Service{
			{Name: "hooks", Plugin: "hooks@market", Command: []string{"bin/hooks", "serve"}, Env: map[string]string{"URL": "http://127.0.0.1:${PORT}"}},
			{Name: "cookiesync", Command: []string{"cookiesync", "supervise"}},
		},
		Configure: Configure{Env: []string{"PORT"}, Run: []string{"mkdir -p ~/.example"}},
		Profiles: map[string]Profile{
			"stack": {Tools: []Artifact{{Name: "kind", Version: "0.29.0", URL: "https://example.com/kind", SHA256: digest, Format: Binary}}},
		},
	}
}

func TestLoadExample(t *testing.T) {
	inventory, err := Load("../../examples/inventory.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(inventory.System), 5; got != want {
		t.Errorf("system artifacts = %d, want %d", got, want)
	}
	if got, want := len(inventory.Tools), 7; got != want {
		t.Errorf("tools = %d, want %d", got, want)
	}
	if got, want := len(inventory.Profiles["stack"].Tools), 2; got != want {
		t.Errorf("stack tools = %d, want %d", got, want)
	}
	if got, want := inventory.Image.User, "agent"; got != want {
		t.Errorf("image.user = %q, want %q", got, want)
	}
	if got, want := inventory.Configure.Env, []string{"EXAMPLE_PORT"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("configure.env = %v, want %v", got, want)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("version: 1\ntoolz: []\n"))
	if err == nil || !strings.Contains(err.Error(), "field toolz not found") {
		t.Fatalf("Parse error = %v, want unknown field toolz", err)
	}
}

func TestValidInventoryValidates(t *testing.T) {
	if err := validInventory().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inventory)
		want   string
	}{
		{"schema version", func(i *Inventory) { i.Version = 2 }, "version is 2, want 1"},
		{"unpinned base image", func(i *Inventory) { i.Image.Base = "ubuntu:24.04" }, `image.base "ubuntu:24.04" is not pinned by @sha256 digest`},
		{"bad user", func(i *Inventory) { i.Image.User = "Root User" }, `image.user "Root User" is not a Linux user name`},
		{"apt name", func(i *Inventory) { i.Apt.T64 = []string{"lib asound"} }, `apt.t64: "lib asound" is not a Debian package name`},
		{"missing digest", func(i *Inventory) { i.Tools[0].SHA256 = "" }, "tools[jq]: set exactly one of sha256 and sha512"},
		{"two digests", func(i *Inventory) { i.Tools[0].SHA512 = digest + digest }, "tools[jq]: set exactly one of sha256 and sha512"},
		{"short sha256", func(i *Inventory) { i.Tools[0].SHA256 = "abc" }, "tools[jq]: sha256 is not 64 lowercase hex digits"},
		{"plain http", func(i *Inventory) { i.Tools[0].URL = "http://example.com/jq" }, `tools[jq]: url "http://example.com/jq" is not an https URL`},
		{"unknown format", func(i *Inventory) { i.Tools[0].Format = "rpm" }, `tools[jq]: format "rpm" is not one of binary, gzip, tar.gz, tar.xz, zip, deb`},
		{"deb under tools", func(i *Inventory) { i.Tools[0].Format = Deb }, "tools[jq]: a deb installs system-wide, so it belongs under system"},
		{"archive without bins", func(i *Inventory) { i.Tools[1].Bins = nil }, "tools[cookiesync]: an archive needs bins naming the members to link"},
		{"escaping member", func(i *Inventory) { i.Tools[1].Bins = map[string]string{"cookiesync": "../cookiesync"} }, `tools[cookiesync]: bin "cookiesync" member "../cookiesync" is not a clean relative path`},
		{"binary renamed member", func(i *Inventory) { i.Tools[0].Bins = map[string]string{"jq": "other"} }, `tools[jq]: bin "jq" must point at "jq", the single file a binary artifact installs`},
		{"absolute dest", func(i *Inventory) { i.Tools[0].Dest = "/opt/jq" }, `tools[jq]: dest "/opt/jq" is not a clean path relative to $HOME`},
		{"system dest", func(i *Inventory) { i.System[1].Dest = "claude" }, "system[claude]: dest applies only to tools"},
		{"duplicate user bin", func(i *Inventory) { i.Tools = append(i.Tools, i.Tools[0]) }, `tools[jq]: bin "jq" is already installed by tools[jq]`},
		{"profile bin clash", func(i *Inventory) { i.Profiles["stack"].Tools[0].Name = "jq" }, `profiles.stack.tools[jq]: bin "jq" is already installed by tools[jq]`},
		{"link without system bin", func(i *Inventory) { i.Links = []string{"jq"} }, `links: "jq" is not a system bin`},
		{"python without uv", func(i *Inventory) { i.System = i.System[1:] }, "python: no system or tools artifact provides the uv bin"},
		{"python version and marketplace", func(i *Inventory) { i.Python.User[0].Version = "1.0.0" }, "python.user[wlm]: set exactly one of version and marketplace"},
		{"system python from marketplace", func(i *Inventory) {
			i.Python.System[0].Version = ""
			i.Python.System[0].Marketplace = "market"
		}, "python.system[cc-transcript]: marketplace checkouts live in the user's home, so only python.user may install from one"},
		{"python unknown marketplace", func(i *Inventory) { i.Python.User[0].Marketplace = "other" }, `python.user[wlm]: marketplace "other" is not under claude.marketplaces`},
		{"plugins without claude", func(i *Inventory) {
			i.System = i.System[:1]
			i.Links = nil
			i.CodexRuntime = nil
		}, "claude: no system or tools artifact provides the claude bin"},
		{"short ref", func(i *Inventory) { i.Claude.Marketplaces[0].Ref = "main" }, `claude.marketplaces[market]: ref "main" is not a 40-digit commit`},
		{"plugin unknown marketplace", func(i *Inventory) { i.Claude.Plugins[0].ID = "hooks@elsewhere" }, `claude.plugins[hooks@elsewhere]: marketplace "elsewhere" is not under claude.marketplaces`},
		{"undeclared env", func(i *Inventory) { i.Claude.Env["DOMAIN"] = "${HOST}" }, "claude.env: DOMAIN references ${HOST}, which configure.env does not declare"},
		{"codex runtime without codex", func(i *Inventory) { i.System = i.System[:2] }, "codexRuntime: no system or tools artifact provides the codex bin"},
		{"captain hook plugin", func(i *Inventory) { i.CaptainHook.Plugin = "other@market" }, `captainHook: plugin "other@market" is not under claude.plugins`},
		{"cookiesync without bin", func(i *Inventory) { i.Tools = i.Tools[:1] }, "cookiesync: no system or tools artifact provides the cookiesync bin"},
		{"service env undeclared", func(i *Inventory) { i.Configure.Env = nil }, "services[hooks].env: URL references ${PORT}, which configure.env does not declare"},
		{"service path command", func(i *Inventory) { i.Services[1].Command = []string{"/usr/bin/cookiesync"} }, `services[cookiesync]: command "/usr/bin/cookiesync" is not a bin name on PATH`},
		{"service unknown plugin", func(i *Inventory) { i.Services[0].Plugin = "other@market" }, `services[hooks]: plugin "other@market" is not under claude.plugins`},
		{"empty configure step", func(i *Inventory) { i.Configure.Run = []string{" "} }, "configure.run: a step is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := validInventory()
			tt.mutate(&inventory)
			err := inventory.Validate()
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Validate() = %v, want %q", err, tt.want)
			}
		})
	}
}
