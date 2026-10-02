package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"it's", `'it'\''s'`},
		{"$HOME `id`", "'$HOME `id`'"},
		{"", "''"},
	}
	for _, tt := range tests {
		if got := quote(tt.in); got != tt.want {
			t.Errorf("quote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestExpand(t *testing.T) {
	tests := []struct{ in, want string }{
		{"mcp.example.com", `"mcp.example.com"`},
		{"http://127.0.0.1:${PORT}", `"http://127.0.0.1:${PORT}"`},
		{"${A}${B}", `"${A}${B}"`},
		{`$(id) "q" \ ` + "`x`", `"\$(id) \"q\" \\ \` + "`x\\`" + `"`},
		{"$PORT ${1}", `"\$PORT \${1}"`},
	}
	for _, tt := range tests {
		if got := expand(tt.in); got != tt.want {
			t.Errorf("expand(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestInstallCall(t *testing.T) {
	tests := []struct {
		name     string
		artifact Artifact
		want     string
	}{
		{
			"binary",
			Artifact{Name: "jq", Version: "1.8.2", URL: "https://example.com/jq", SHA256: digest, Format: Binary},
			`install_artifact 'jq' '1.8.2' 'https://example.com/jq' sha256 '` + digest + `' binary "$tool_dir/"'jq-1.8.2' "$bin_dir" 'jq' 'jq'`,
		},
		{
			"archive with dest",
			Artifact{Name: "chrome", Version: "1.0", URL: "https://example.com/c.zip", SHA256: digest, Format: Zip, Dest: ".browsers/chrome-1.0", Bins: map[string]string{"google-chrome": "linux/chrome", "chromedriver": "linux/driver"}},
			`install_artifact 'chrome' '1.0' 'https://example.com/c.zip' sha256 '` + digest + `' zip "$HOME/"'.browsers/chrome-1.0' "$bin_dir" 'chromedriver' 'linux/driver' 'google-chrome' 'linux/chrome'`,
		},
		{
			"deb",
			Artifact{Name: "orca", Version: "1.4.215", URL: "https://example.com/orca.deb", SHA512: digest + digest, Format: Deb, Bins: map[string]string{"orca": "/opt/Orca/orca-ide"}},
			`install_artifact 'orca' '1.4.215' 'https://example.com/orca.deb' sha512 '` + digest + digest + `' deb "$tool_dir/"'orca-1.4.215' "$bin_dir" 'orca' '/opt/Orca/orca-ide'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := installCall(tt.artifact, "tool_dir", "bin_dir"); got != tt.want {
				t.Errorf("installCall =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestVerifyCalls(t *testing.T) {
	artifact := Artifact{Name: "uv", Version: "0.1.0", URL: "https://example.com/uv.tar.gz", SHA256: digest, Format: TarGz, Bins: map[string]string{"uvx": "uv/uvx", "uv": "uv/uv"}, Verify: []string{"--version"}}
	want := []string{
		`verify_pin "$system_tool_dir/"'uv-0.1.0' '` + digest + `'`,
		`verify_link "$system_bin_dir/"'uv' "$system_tool_dir/"'uv-0.1.0/uv/uv' '--version'`,
		`verify_link "$system_bin_dir/"'uvx' "$system_tool_dir/"'uv-0.1.0/uv/uvx' '--version'`,
	}
	if got := verifyCalls(artifact, "system_tool_dir", "system_bin_dir"); !slices.Equal(got, want) {
		t.Errorf("verifyCalls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderScopesProfileTools(t *testing.T) {
	inventory := validInventory()
	agents, err := Render(inventory, "agents")
	if err != nil {
		t.Fatalf("Render agents: %v", err)
	}
	stack, err := Render(inventory, "stack")
	if err != nil {
		t.Fatalf("Render stack: %v", err)
	}
	kind := `queue_artifact install_artifact 'kind' '0.29.0'`
	if bytes.Contains(agents.Plugins, []byte(kind)) {
		t.Errorf("agents plugins.sh installs kind")
	}
	if !bytes.Contains(stack.Plugins, []byte(kind)) {
		t.Errorf("stack plugins.sh does not install kind")
	}
	packKind := `  pack_path required "$user_home/"'.local/share/cc-remote/tools/kind-0.29.0'` + "\n"
	if bytes.Contains(agents.ProvisionScript, []byte(packKind)) {
		t.Errorf("agents provision.sh packs kind")
	}
	if !bytes.Equal(bytes.Replace(stack.ProvisionScript, []byte(packKind), nil, 1), agents.ProvisionScript) {
		t.Errorf("provision.sh differs between profiles beyond packing kind")
	}
	if agents.Fingerprint() == stack.Fingerprint() {
		t.Errorf("profiles with different tools share fingerprint %s", agents.Fingerprint())
	}
	if !slices.Equal(stack.Env, []string{"PORT"}) {
		t.Errorf("Env = %v, want [PORT]", stack.Env)
	}
}

func TestRenderPluginsServicesAndEnv(t *testing.T) {
	scripts, err := Render(validInventory(), "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`  require_env 'PORT'`,
		`  claude_env 'DOMAIN' "example.com"`,
		`  executable="$(plugin_path "$plugins" 'hooks@market')/"'bin/hooks'`,
		`  service 'hooks' env "URL=http://127.0.0.1:${PORT}" "$executable" 'serve'`,
		`  executable="$(command -v 'cookiesync')"`,
		`  checkout 'market' 'owner/market' '` + commit + `' public`,
		`  install_uv_launcher 'example-tool' '3.13' 'example-tool[cli] @ git+file://'"$marketplace_dir"'/market@` + commit + `'`,
		`  verify_uv_launcher 'example-tool' '3.13' 'example-tool[cli] @ git+file://'"$marketplace_dir"'/market@` + commit + `'`,
		`  install_captain_hook '1.0.0' 'https://example.com/hook.tar.gz' '` + digest + `'`,
		`  synckit_state '` + digest + `'`,
		"hooks@market 1.0.0\nPINS",
	} {
		if !bytes.Contains(scripts.Plugins, []byte(want+"\n")) {
			t.Errorf("plugins.sh lacks line %q", want)
		}
	}
	if want := "  mkdir -p ~/.example\n"; !bytes.Contains(scripts.Plugins, []byte(want)) {
		t.Errorf("plugins.sh lacks the configure step %q", want)
	}
	if want := "  (\n    cd \"$HOME\"\n    mkdir -p ~/.cache/example\n  )\n  local link_check=spelling\n  verify_user\n}\n"; !bytes.Contains(scripts.Plugins, []byte(want)) {
		t.Errorf("plugins.sh lacks the prepare block %q", want)
	}
	if want := "  local stamp=\"${1:?publish needs the ready stamp}\"\n  verify_system\n  verify_user_links\n  mkdir -p \"$state_dir\"\n"; !bytes.Contains(scripts.Plugins, []byte(want)) {
		t.Errorf("plugins.sh lacks the publish checks %q", want)
	}
}

func TestRenderExposesPayloadTrees(t *testing.T) {
	inventory := validInventory()
	inventory.Claude.Marketplaces = append(inventory.Claude.Marketplaces, Marketplace{Name: "secret", GitHub: "owner/secret", Ref: commit, Private: true})
	inventory.Claude.Plugins = append(inventory.Claude.Plugins, Plugin{ID: "vault@secret", Version: "2.0.0"})
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	system := `  expose link required '/opt/cc-remote/tools/uv-0.1.0'
  expose link required '/opt/cc-remote/tools/claude-2.0.0'
  expose link required '/opt/cc-remote/tools/codex-0.2.0'
  expose link required '/opt/uv/tools/cc-transcript'
  expose copy required '/usr/local/bin/cc-transcript'
  expose link required '/opt/uv/python'
  drain_artifacts
}
`
	pack := `  version="$(os_version)"
  pack_path required '/opt/cc-remote/tools/uv-0.1.0'
  pack_path required '/opt/cc-remote/tools/claude-2.0.0'
  pack_path required '/opt/cc-remote/tools/codex-0.2.0'
  pack_path required '/opt/uv/tools/cc-transcript'
  pack_path required '/usr/local/bin/cc-transcript'
  pack_path required '/opt/uv/python'
  pack_path required "$user_home/"'.local/share/cc-remote/tools/jq-1.8.2'
  pack_path required "$user_home/"'.local/share/cc-remote/tools/cookiesync-0.30.0'
  pack_path required "$user_home/"'.local/share/cc-remote/marketplaces/market'
  pack_path required "$user_home/"'.local/share/cc-remote/marketplaces/secret'
  pack_path required "$user_home/"'.claude/plugins/cache/market/hooks/1.0.0'
  pack_path required "$user_home/"'.claude/plugins/cache/secret/vault/2.0.0'
  pack_path required "$user_home/"'.claude/plugins/installed_plugins.json'
  pack_path required "$user_home/"'.claude/plugins/known_marketplaces.json'
  pack_path required "$user_home/"'.claude/settings.json'
  pack_path required "$user_home/"'.cache/codex-runtimes/codex-primary-runtime'
  pack_path required "$user_home/"'.codex/config.toml'
  pack_path required "$user_home/"'.codex/plugins/cache/openai-primary-runtime'
  pack_path required "$user_home/"'.daemonkit/tools/capt-hook/1.0.0'
  pack_path optional "$user_home/"'.local/share/uv/python'
  pack_native "$user_home/"'.claude/plugins/cache/market/hooks/1.0.0' 'bin/hooks'
  jq -n `
	for _, want := range []string{system, pack} {
		if !bytes.Contains(scripts.ProvisionScript, []byte(want)) {
			t.Errorf("provision.sh lacks\n%s", want)
		}
	}
	home := `  if [ "$#" -gt 0 ]; then
    local payload="$1"
    expose link required "$HOME/"'.local/share/cc-remote/tools/jq-1.8.2'
    expose link required "$HOME/"'.local/share/cc-remote/tools/cookiesync-0.30.0'
    expose link required "$HOME/"'.local/share/cc-remote/marketplaces/market'
    expose link required "$HOME/"'.local/share/cc-remote/marketplaces/secret'
    expose copy required "$HOME/"'.claude/plugins/cache/market/hooks/1.0.0'
    expose copy required "$HOME/"'.claude/plugins/cache/secret/vault/2.0.0'
    expose copy required "$HOME/"'.claude/plugins/installed_plugins.json'
    expose copy required "$HOME/"'.claude/plugins/known_marketplaces.json'
    expose copy required "$HOME/"'.claude/settings.json'
    expose link required "$HOME/"'.cache/codex-runtimes/codex-primary-runtime'
    expose copy required "$HOME/"'.codex/config.toml'
    expose copy required "$HOME/"'.codex/plugins/cache/openai-primary-runtime'
    expose children required "$HOME/"'.daemonkit/tools/capt-hook/1.0.0'
    expose link optional "$HOME/"'.local/share/uv/python'
    native_home
    expose_native "$HOME/"'.claude/plugins/cache/market/hooks/1.0.0' 'bin/hooks'
  fi
`
	stale := `  if stale_marketplace "$working" 'hooks@market 1.0.0'; then
    claude plugin marketplace update 'market'
  fi
  if stale_marketplace "$working" 'vault@secret 2.0.0'; then
    claude plugin marketplace update 'secret'
  fi
  install_plugin 'hooks@market' '1.0.0' "$1"
  install_plugin 'vault@secret' '2.0.0' "$1"
`
	natives := `run_natives() {
  :
  native_home
  prove_native 'hooks@market' 'bin/hooks' '` + commit + `' "$HOME/"'.claude/plugins/cache/market/hooks/1.0.0'
  prepare_native 'hooks@market' 'bin/hooks' '` + commit + `' "$HOME/"'.claude/plugins/cache/market/hooks/1.0.0'
}
`
	for _, want := range []string{home, stale, natives} {
		if !bytes.Contains(scripts.Plugins, []byte(want)) {
			t.Errorf("plugins.sh lacks\n%s", want)
		}
	}
}

func TestNativesSelectsPinnedLaunchers(t *testing.T) {
	inventory := Inventory{
		Version: SchemaVersion,
		Claude: Claude{
			Marketplaces: []Marketplace{{Name: "market", GitHub: "owner/market", Ref: commit}, {Name: "official", GitHub: "owner/official", Branch: "main"}},
			Plugins: []Plugin{
				{ID: "hooks@market", Version: "1.0.0", Bins: []string{"bin/hooks", "bin/hooks-ctl"}},
				{ID: "captain-hook@market", Version: "2.0.0", Bins: []string{"bin/capt-hook"}},
				{ID: "notes@market", Version: "3.0.0"},
				{ID: "datadog@official", Version: "0.7.17", Bins: []string{"bin/dd"}},
			},
		},
		Services: []Service{
			{Name: "hooks", Plugin: "hooks@market", Command: []string{"bin/hooks", "serve"}},
			{Name: "notes", Plugin: "notes@market", Command: []string{"bin/notesd"}},
			{Name: "cookiesync", Command: []string{"cookiesync", "supervise"}},
		},
	}
	want := []native{
		{"hooks@market", commit, ".claude/plugins/cache/market/hooks/1.0.0", "bin/hooks"},
		{"hooks@market", commit, ".claude/plugins/cache/market/hooks/1.0.0", "bin/hooks-ctl"},
		{"notes@market", commit, ".claude/plugins/cache/market/notes/3.0.0", "bin/notesd"},
	}
	if got := natives(inventory); !slices.Equal(got, want) {
		t.Errorf("natives = %v, want %v", got, want)
	}
}

func TestHomeTreesForBranchMarketplacesWithoutPlugins(t *testing.T) {
	inventory := Inventory{Claude: Claude{Marketplaces: []Marketplace{{Name: "official", GitHub: "owner/official", Branch: "main"}}}}
	want := []tree{
		{exposeCopy, required, ".claude/plugins/marketplaces/official"},
		{exposeCopy, required, ".claude/plugins/known_marketplaces.json"},
		{exposeCopy, required, ".claude/settings.json"},
	}
	if got := homeTrees(inventory, nil); !slices.Equal(got, want) {
		t.Errorf("homeTrees = %v, want %v", got, want)
	}
}

func TestRenderPacksClaudeSettings(t *testing.T) {
	ref := Marketplace{Name: "market", GitHub: "owner/market", Ref: commit}
	branch := Marketplace{Name: "official", GitHub: "owner/official", Branch: "main"}
	tests := []struct {
		name         string
		marketplaces []Marketplace
		plugins      []Plugin
		want         int
	}{
		{"plugins with ref marketplaces only", []Marketplace{ref}, []Plugin{{ID: "hooks@market", Version: "1.0.0"}}, 1},
		{"plugins with a branch marketplace", []Marketplace{ref, branch}, []Plugin{{ID: "hooks@market", Version: "1.0.0"}, {ID: "docs@official", Version: "2.0.0"}}, 1},
		{"branch marketplace without plugins", []Marketplace{branch}, nil, 1},
		{"ref marketplace without plugins", []Marketplace{ref}, nil, 0},
	}
	const (
		expose    = `    expose copy required "$HOME/"'.claude/settings.json'` + "\n"
		pack      = `  pack_path required "$user_home/"'.claude/settings.json'` + "\n"
		packKnown = `  pack_path required "$user_home/"'.claude/plugins/known_marketplaces.json'` + "\n"
	)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := Inventory{Version: SchemaVersion, Claude: Claude{Marketplaces: tt.marketplaces, Plugins: tt.plugins}}
			scripts, err := Render(inventory, "agents")
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if !bytes.Contains(scripts.ProvisionScript, []byte(packKnown)) {
				t.Fatalf("provision.sh lacks %q", packKnown)
			}
			if got := bytes.Count(scripts.Plugins, []byte(expose)); got != tt.want {
				t.Errorf("plugins.sh exposes settings.json %d times, want %d", got, tt.want)
			}
			if got := bytes.Count(scripts.ProvisionScript, []byte(pack)); got != tt.want {
				t.Errorf("provision.sh packs settings.json %d times, want %d", got, tt.want)
			}
		})
	}
}

func TestUVToolDir(t *testing.T) {
	tests := []struct {
		tool PythonTool
		want string
	}{
		{PythonTool{Name: "cc-transcript"}, "cc-transcript"},
		{PythonTool{Name: "writer", Package: "writer-cli"}, "writer-cli"},
		{PythonTool{Name: "writer", Package: "writer[lab,scoring]"}, "writer"},
	}
	for _, tt := range tests {
		if got := uvToolDir(tt.tool); got != tt.want {
			t.Errorf("uvToolDir(%+v) = %q, want %q", tt.tool, got, tt.want)
		}
	}
}

func TestRenderAddsProfilePrepareSteps(t *testing.T) {
	scripts, err := Render(validInventory(), "stack")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "    mkdir -p ~/.cache/example\n    kind version\n  )\n"; !bytes.Contains(scripts.Plugins, []byte(want)) {
		t.Errorf("stack plugins.sh lacks the prepare steps %q", want)
	}
}

func TestRenderProvisionWithoutOptionalSections(t *testing.T) {
	inventory := Inventory{Version: SchemaVersion}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, absent := range []string{"managed-settings.json", "uv\" tool install", "install_artifact '"} {
		if bytes.Contains(scripts.ProvisionScript, []byte(absent)) {
			t.Errorf("empty inventory's provision.sh contains %q", absent)
		}
	}
	if _, err := RenderImage(inventory); err == nil || err.Error() != "inventory has no image section" {
		t.Errorf("RenderImage error = %v, want no image section", err)
	}
}

func TestFingerprintFormat(t *testing.T) {
	scripts := Scripts{ProvisionScript: []byte("A"), Plugins: []byte("BC")}
	sum := sha256.Sum256([]byte("cc-remote/tools/v1\nprovision.sh 1\nAplugins.sh 2\nBC"))
	if got, want := scripts.Fingerprint(), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Scripts.Fingerprint = %s, want %s", got, want)
	}
	context := Context{Dockerfile: []byte("D"), Provision: []byte("A"), Start: []byte("S")}
	sum = sha256.Sum256([]byte("cc-remote/image/v1\nDockerfile 1\nDprovision.sh 1\nAstart.sh 1\nS"))
	if got, want := context.Fingerprint(), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Context.Fingerprint = %s, want %s", got, want)
	}
}

func TestStamp(t *testing.T) {
	scripts := Scripts{ProvisionScript: []byte("A"), Plugins: []byte("BC")}
	image := Context{Dockerfile: []byte("D"), Provision: []byte("A"), Start: []byte("S")}
	tools, img := scripts.Fingerprint(), image.Fingerprint()
	tests := []struct {
		name    string
		image   *Context
		payload string
		framed  string
	}{
		{"tools only", nil, "", "tools 64\n" + tools},
		{"an image", &image, "", "tools 64\n" + tools + "image 64\n" + img},
		{"a payload", nil, digest, "tools 64\n" + tools + "payload 64\n" + digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := sha256.Sum256([]byte("cc-remote/ready/v1\n" + tt.framed))
			if got, want := Stamp(scripts, tt.image, tt.payload), hex.EncodeToString(sum[:]); got != want {
				t.Errorf("Stamp = %s, want %s", got, want)
			}
		})
	}
}

func TestFingerprintsTrackTheirInputs(t *testing.T) {
	base := fingerprints(t, validInventory())
	if again := fingerprints(t, validInventory()); again != base {
		t.Fatalf("fingerprints are not deterministic: %v then %v", base, again)
	}
	tests := []struct {
		name               string
		mutate             func(*Inventory)
		toolsMove, imgMove bool
	}{
		{"user tool digest", func(i *Inventory) { i.Tools[0].SHA256 = strings.Repeat("3", 64) }, true, false},
		{"user tool version", func(i *Inventory) { i.Tools[0].Version = "1.8.3" }, true, false},
		{"system tool version", func(i *Inventory) { i.System[1].Version = "2.0.1" }, true, true},
		{"plugin version", func(i *Inventory) { i.Claude.Plugins[0].Version = "1.0.1" }, true, false},
		{"marketplace ref", func(i *Inventory) { i.Claude.Marketplaces[0].Ref = strings.Repeat("4", 40) }, true, false},
		{"captain hook version", func(i *Inventory) { i.CaptainHook.Version = "1.0.1" }, true, false},
		{"image user", func(i *Inventory) { i.Image.User = "dev" }, false, true},
		{"image base", func(i *Inventory) { i.Image.Base = "ubuntu:26.04@sha256:" + strings.Repeat("5", 64) }, false, true},
		{"image name", func(i *Inventory) { i.Image.Name = "other" }, false, false},
		{"image layer", func(i *Inventory) { i.Image.Layer = append(i.Image.Layer, "RUN true") }, false, true},
		{"prepare step", func(i *Inventory) { i.Prepare = []string{"true"} }, true, false},
		{"apt payload", func(i *Inventory) { i.Apt.Payload = &AptPayload{Closure: []string{"libnss3"}} }, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := validInventory()
			tt.mutate(&inventory)
			got := fingerprints(t, inventory)
			if moved := got.tools != base.tools; moved != tt.toolsMove {
				t.Errorf("tool fingerprint moved = %v, want %v", moved, tt.toolsMove)
			}
			if moved := got.image != base.image; moved != tt.imgMove {
				t.Errorf("image fingerprint moved = %v, want %v", moved, tt.imgMove)
			}
			if moved := got.stamp != base.stamp; moved != tt.toolsMove {
				t.Errorf("Sprites ready stamp moved = %v, want %v", moved, tt.toolsMove)
			}
		})
	}
}

func TestOnlyAnAptPayloadRendersTheClosure(t *testing.T) {
	plain := validInventory()
	payload := validInventory()
	payload.Apt.Payload = &AptPayload{Resident: []string{"bubblewrap"}, Closure: []string{"libnss3"}, Bins: []string{"certutil"}, Projections: []string{"/usr/share/X11/xkb"}}
	for name, inv := range map[string]Inventory{"plain": plain, "image": payload} {
		scripts, err := Render(inv, "agents")
		if err != nil {
			t.Fatalf("Render %s: %v", name, err)
		}
		context, err := RenderImage(inv)
		if err != nil {
			t.Fatalf("RenderImage %s: %v", name, err)
		}
		for _, rendered := range [][]byte{context.Provision, context.Dockerfile} {
			if bytes.Contains(rendered, []byte("closure")) || bytes.Contains(rendered, []byte("nosuid")) {
				t.Errorf("the %s image context renders the closure", name)
			}
		}
		if name == "plain" {
			for _, rendered := range [][]byte{scripts.ProvisionScript, scripts.Plugins} {
				if bytes.Contains(rendered, []byte("closure")) || bytes.Contains(rendered, []byte("nosuid")) || bytes.Contains(rendered, []byte("loader")) {
					t.Errorf("an inventory without apt.payload renders the closure")
				}
			}
			if !bytes.Contains(scripts.ProvisionScript, []byte("mount -t squashfs -o ro,loop ")) || !bytes.Contains(scripts.ProvisionScript, []byte("  packages) provision_packages ;;\n")) {
				t.Errorf("an inventory without apt.payload changed the payload mount or the packages phase")
			}
		}
	}
	with, err := Render(payload, "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"closure_packages=('libnss3')\n",
		"  printf '%s\\n' 'bubblewrap'\n",
		"  expose link required '/opt/cc-remote/closure'\n  expose copy required '/usr/local/bin/certutil'\n  expose copy required '/usr/local/share/mime'\n  expose copy required '/usr/local/share/glib-2.0/schemas'\n  expose copy required '/usr/local/share/icons'\n",
		"  closure_link \"$closure_root\" \"$payload$closure_root\"\n  closure_link '/usr/local/bin/certutil' \"$closure_root/\"'usr/bin/certutil'\n",
		"  closure_project '/usr/share/X11/xkb'\n  write_conf \"$closure_loader_conf\"",
		"mount -t squashfs -o ro,nosuid,nodev,loop \"$image\" \"$dir\"\n  fi\ndone\nldconfig\nSH\n",
		"  pack_path required '/opt/cc-remote/closure'\n  pack_path required '/usr/local/bin/certutil'\n",
		"  packages) provision_packages \"${2:-}\" ;;\n  loader) provision_loader ;;\n",
	} {
		if !bytes.Contains(with.ProvisionScript, []byte(want)) {
			t.Errorf("provision.sh with apt.payload lacks\n%s", want)
		}
	}
	if !bytes.Contains(with.Plugins, []byte("  verify_closure_bin 'certutil'\n")) {
		t.Errorf("plugins.sh with apt.payload does not verify the exposed bins")
	}
}

type fingerprintSet struct{ tools, image, stamp string }

func fingerprints(t *testing.T, inventory Inventory) fingerprintSet {
	t.Helper()
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	context, err := RenderImage(inventory)
	if err != nil {
		t.Fatalf("RenderImage: %v", err)
	}
	return fingerprintSet{scripts.Fingerprint(), context.Fingerprint(), Stamp(scripts, nil, digest)}
}

func TestRenderImageDockerfile(t *testing.T) {
	context, err := RenderImage(validInventory())
	if err != nil {
		t.Fatalf("RenderImage: %v", err)
	}
	want := "FROM ubuntu:24.04@sha256:" + digest + `

RUN useradd -m -s /bin/bash agent
COPY provision.sh start.sh /tmp/cc-remote/
RUN bash /tmp/cc-remote/provision.sh packages \
    && bash /tmp/cc-remote/provision.sh tools \
    && install -m 0755 /tmp/cc-remote/start.sh /usr/local/bin/cc-remote-start \
    && rm -rf /tmp/cc-remote
ENV LANG=C.UTF-8

USER agent
ENV PATH=/home/agent/.local/bin:${PATH}
`
	if got := string(context.Dockerfile); got != want {
		t.Errorf("Dockerfile =\n%s\nwant\n%s", got, want)
	}
	scripts, err := Render(validInventory(), "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var system []string
	for line := range strings.Lines(string(scripts.ProvisionScript)) {
		if (!strings.HasPrefix(line, "  pack_path ") && !strings.HasPrefix(line, "  pack_native ")) || !strings.Contains(line, `"$user_home/"`) {
			system = append(system, line)
		}
	}
	if got, want := string(context.Provision), strings.Join(system, ""); got != want {
		t.Errorf("the image context's provision.sh is not the rendered one without its user-home packing:\n%s", got)
	}
}
