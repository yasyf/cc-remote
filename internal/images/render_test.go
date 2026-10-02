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
	if !bytes.Equal(agents.Provision, stack.Provision) {
		t.Errorf("provision.sh differs between profiles")
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
		`  executable="$(plugin_root 'hooks@market')/"'bin/hooks'`,
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
	if want := "  (\n    cd \"$HOME\"\n    mkdir -p ~/.cache/example\n  )\n  run_verify\n"; !bytes.Contains(scripts.Plugins, []byte(want)) {
		t.Errorf("plugins.sh lacks the prepare block %q", want)
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
		if bytes.Contains(scripts.Provision, []byte(absent)) {
			t.Errorf("empty inventory's provision.sh contains %q", absent)
		}
	}
	if _, err := RenderImage(inventory); err == nil || err.Error() != "inventory has no image section" {
		t.Errorf("RenderImage error = %v, want no image section", err)
	}
}

func TestFingerprintFormat(t *testing.T) {
	scripts := Scripts{Provision: []byte("A"), Plugins: []byte("BC")}
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
	scripts := Scripts{Provision: []byte("A"), Plugins: []byte("BC")}
	image := Context{Dockerfile: []byte("D"), Provision: []byte("A"), Start: []byte("S")}
	tools, img := scripts.Fingerprint(), image.Fingerprint()
	sum := sha256.Sum256([]byte("cc-remote/ready/v1\ntools 64\n" + tools))
	if got, want := Stamp(scripts, nil), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Stamp without image = %s, want %s", got, want)
	}
	sum = sha256.Sum256([]byte("cc-remote/ready/v1\ntools 64\n" + tools + "image 64\n" + img))
	if got, want := Stamp(scripts, &image), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Stamp with image = %s, want %s", got, want)
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
		{"system tool version", func(i *Inventory) { i.System[1].Version = "2.0.1" }, true, true},
		{"plugin version", func(i *Inventory) { i.Claude.Plugins[0].Version = "1.0.1" }, true, false},
		{"marketplace ref", func(i *Inventory) { i.Claude.Marketplaces[0].Ref = strings.Repeat("4", 40) }, true, false},
		{"image user", func(i *Inventory) { i.Image.User = "dev" }, false, true},
		{"image base", func(i *Inventory) { i.Image.Base = "ubuntu:26.04@sha256:" + strings.Repeat("5", 64) }, false, true},
		{"image name", func(i *Inventory) { i.Image.Name = "other" }, false, false},
		{"image layer", func(i *Inventory) { i.Image.Layer = append(i.Image.Layer, "RUN true") }, false, true},
		{"prepare step", func(i *Inventory) { i.Prepare = []string{"true"} }, true, false},
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
		})
	}
}

type fingerprintPair struct{ tools, image string }

func fingerprints(t *testing.T, inventory Inventory) fingerprintPair {
	t.Helper()
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	context, err := RenderImage(inventory)
	if err != nil {
		t.Fatalf("RenderImage: %v", err)
	}
	return fingerprintPair{scripts.Fingerprint(), context.Fingerprint()}
}

func TestRenderImageDockerfile(t *testing.T) {
	context, err := RenderImage(validInventory())
	if err != nil {
		t.Fatalf("RenderImage: %v", err)
	}
	want := "FROM ubuntu:24.04@sha256:" + digest + `

RUN useradd -m -s /bin/bash agent
COPY provision.sh start.sh /tmp/cc-remote/
RUN bash /tmp/cc-remote/provision.sh \
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
	if !bytes.Equal(context.Provision, scripts.Provision) {
		t.Errorf("the image context's provision.sh differs from the rendered one")
	}
}
