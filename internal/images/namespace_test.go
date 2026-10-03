package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	pushedDigest = "sha256:78df9f0f523cff1f3bee75815f639a35904889dec22d8d8ffa161296a3608d06"
	buildToken   = "ghs_buildtoken123"
)

type namespaceCall struct {
	name string
	args []string
}

type fakeNamespace struct {
	t        *testing.T
	calls    []namespaceCall
	files    map[string]string
	secret   string
	mode     os.FileMode
	content  string
	failPush error
	registry string
}

func (f *fakeNamespace) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, namespaceCall{name, args})
	if name != nsc || args[0] != "build" {
		return nil
	}
	root := args[1]
	dockerfile := args[slices.Index(args, "--file")+1]
	raw, err := os.ReadFile(filepath.Join(root, filepath.Clean("/"+dockerfile)))
	if err != nil {
		return fmt.Errorf("the dockerfile %q does not resolve under the context root %s: %w", dockerfile, root, err)
	}
	f.files = map[string]string{"--file": string(raw)}
	for _, file := range []string{"Dockerfile", "provision.sh", "start.sh", "plugins.sh", "finalize.sh", "baked.json"} {
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return err
		}
		f.files[file] = string(raw)
	}
	if i := slices.Index(args, "--secret"); i >= 0 {
		f.secret = strings.TrimPrefix(args[i+1], "id=github-token,src=")
		info, err := os.Stat(f.secret)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(f.secret)
		if err != nil {
			return err
		}
		f.mode, f.content = info.Mode().Perm(), string(raw)
	}
	return f.failPush
}

func (f *fakeNamespace) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, namespaceCall{name, args})
	switch args[0] {
	case "registry":
		return []byte(`{"image":{"repository":"agent-host-agents","digest":"` + pushedDigest + `","created_at":"2026-10-02T15:36:15Z","sizes":{"total":"1"}}}`), nil
	case "workspace":
		return []byte(f.registry), nil
	}
	f.t.Fatalf("unexpected %s %v", name, args)
	return nil, nil
}

func TestBuildPushesTheBakedContextAndWiresItsDigest(t *testing.T) {
	image, err := RenderImage(validInventory(), "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	cli := &fakeNamespace{t: t, registry: "nscr.io/abc123ws\n"}
	built, err := image.Build(t.Context(), cli, func(context.Context) (string, error) {
		t.Fatal("a public inventory asked for the GitHub token")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := cli.calls[0].args[1]
	fingerprint := image.Fingerprint()
	want := []namespaceCall{
		{nsc, []string{"build", dir, "--file", "Dockerfile", "--platform", "linux/amd64", "--name", "agent-host-agents:" + fingerprint, "--push"}},
		{nsc, []string{"registry", "describe", "--repository", "agent-host-agents", "--reference", fingerprint, "--output", "json"}},
		{nsc, []string{"workspace", "describe", "--key", "registry_url", "--output", "json"}},
		{devbox, []string{
			"image", "wire", "nscr.io/abc123ws/agent-host-agents@" + pushedDigest,
			"--description", "cc-remote agent host cc-remote-image=" + fingerprint,
			"--user", "agent",
			"--on_startup", "/usr/local/bin/cc-remote-start",
			"--workspace_dir", "/workspaces",
			"--persistency", "whole",
		}},
	}
	if len(cli.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", cli.calls, want)
	}
	for i := range want {
		if cli.calls[i].name != want[i].name || !slices.Equal(cli.calls[i].args, want[i].args) {
			t.Errorf("call %d = %s %q, want %s %q", i, cli.calls[i].name, cli.calls[i].args, want[i].name, want[i].args)
		}
	}
	for name, data := range map[string][]byte{"--file": image.Dockerfile, "Dockerfile": image.Dockerfile, "provision.sh": image.Provision, "start.sh": image.Start, "plugins.sh": image.Plugins, "finalize.sh": image.Finalize, "baked.json": image.Manifest} {
		if cli.files[name] != string(data) {
			t.Errorf("context %s differs from the rendered file", name)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Build left its context directory %s behind: %v", dir, err)
	}
	wantBuilt := Built{Profile: "agents", Platform: "linux/amd64", Fingerprint: fingerprint, Repository: "agent-host-agents", Tag: fingerprint, Digest: pushedDigest, Reference: "nscr.io/abc123ws/agent-host-agents@" + pushedDigest}
	manifest := built.Manifest
	built.Manifest = nil
	if !reflect.DeepEqual(built, wantBuilt) {
		t.Errorf("Built = %+v, want %+v", built, wantBuilt)
	}
	if !json.Valid(manifest) || string(manifest) != string(image.Manifest) {
		t.Errorf("the receipt's manifest is not the baked one: %s", manifest)
	}
}

func TestBuildPassesThePrivateTokenOnlyAsATransientSecretFile(t *testing.T) {
	inventory := validInventory()
	inventory.Claude.Marketplaces[0].Private = true
	image, err := RenderImage(inventory, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	token := func(context.Context) (string, error) { return buildToken, nil }
	t.Run("built", func(t *testing.T) {
		cli := &fakeNamespace{t: t, registry: "nscr.io/abc123ws\n"}
		if _, err := image.Build(t.Context(), cli, token); err != nil {
			t.Fatal(err)
		}
		dir := cli.calls[0].args[1]
		if cli.secret == "" || strings.HasPrefix(cli.secret, dir) {
			t.Fatalf("secret file %q is missing or inside the context %s", cli.secret, dir)
		}
		if cli.mode != 0o600 || cli.content != buildToken+"\n" {
			t.Errorf("secret file mode %v holds %d bytes; want 0600 holding the token line", cli.mode, len(cli.content))
		}
		if _, err := os.Stat(cli.secret); !os.IsNotExist(err) {
			t.Errorf("the secret file outlived the build: %v", err)
		}
		for _, call := range cli.calls {
			if strings.Contains(strings.Join(call.args, " "), buildToken) {
				t.Errorf("%s %v carries the token", call.name, call.args)
			}
		}
		for name, data := range cli.files {
			if strings.Contains(data, buildToken) {
				t.Errorf("context file %s carries the token", name)
			}
		}
	})
	t.Run("failed build", func(t *testing.T) {
		cli := &fakeNamespace{t: t, failPush: errors.New("nsc build: exit status 1")}
		_, err := image.Build(t.Context(), cli, token)
		if err == nil || strings.Contains(err.Error(), buildToken) {
			t.Fatalf("Build = %v", err)
		}
		if _, err := os.Stat(cli.secret); !os.IsNotExist(err) {
			t.Errorf("the secret file outlived the failed build: %v", err)
		}
		if len(cli.calls) != 1 {
			t.Errorf("a failed build went on to %v", cli.calls[1:])
		}
	})
	t.Run("no token", func(t *testing.T) {
		cli := &fakeNamespace{t: t}
		_, err := image.Build(t.Context(), cli, func(context.Context) (string, error) { return "", errors.New("gh: not logged in") })
		if err == nil || !strings.Contains(err.Error(), "private marketplace needs the GitHub token") || len(cli.calls) != 0 {
			t.Errorf("Build = %v after %v", err, cli.calls)
		}
	})
	t.Run("multi-line token", func(t *testing.T) {
		cli := &fakeNamespace{t: t}
		_, err := image.Build(t.Context(), cli, func(context.Context) (string, error) { return "a\nb", nil })
		if err == nil || !strings.Contains(err.Error(), "spans lines") || len(cli.calls) != 0 {
			t.Errorf("Build = %v after %v", err, cli.calls)
		}
	})
}

func TestBuildReadsOnlyTheNativeRegistryLine(t *testing.T) {
	tests := []struct {
		name, out, want string
	}{
		{"plain line", "nscr.io/abc123ws\n", "nscr.io/abc123ws"},
		{"json string", `"nscr.io/abc123ws"` + "\n", ""},
		{"json object", `{"registry_url":"nscr.io/abc123ws"}`, ""},
		{"two lines", "nscr.io/abc123ws\nnscr.io/other\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRegistry([]byte(tt.out))
			if tt.want == "" {
				if err == nil {
					t.Errorf("parseRegistry(%q) = %q, want an error", tt.out, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("parseRegistry(%q) = %q, %v", tt.out, got, err)
			}
		})
	}
}

func TestBuildRefusesAnotherRepositoryOrAMutableReference(t *testing.T) {
	image, err := RenderImage(validInventory(), "stack", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		`{"image":{"repository":"agent-host-agents","digest":"` + pushedDigest + `"}}`,
		`{"image":{"repository":"agent-host-stack","digest":"latest"}}`,
		`not json`,
	} {
		if _, err := image.parseDescribed([]byte(out)); err == nil {
			t.Errorf("parseDescribed accepted %s", out)
		}
	}
	if got, err := image.parseDescribed([]byte(`{"image":{"repository":"agent-host-stack","digest":"` + pushedDigest + `"}}`)); err != nil || got != pushedDigest {
		t.Errorf("parseDescribed = %q, %v", got, err)
	}
}

func TestNamespaceCLIPutsTheRegionOnNscOnly(t *testing.T) {
	regional := NamespaceCLI{Region: "eu"}
	if got := regional.Argv(nsc, []string{"build", "/ctx"}); !slices.Equal(got, []string{"--region", "eu", "build", "/ctx"}) {
		t.Errorf("nsc argv = %q", got)
	}
	if got := regional.Argv(devbox, []string{"image", "wire"}); !slices.Equal(got, []string{"image", "wire"}) {
		t.Errorf("devbox argv = %q", got)
	}
	if got := (NamespaceCLI{}).Argv(nsc, []string{"build"}); !slices.Equal(got, []string{"build"}) {
		t.Errorf("unconfigured nsc argv = %q", got)
	}
}

func TestBuildResolvesTheDockerfileAgainstTheContextRoot(t *testing.T) {
	image, err := RenderImage(validInventory(), "agents", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	cli := &fakeNamespace{t: t, registry: "nscr.io/abc123ws\n"}
	built, err := image.Build(t.Context(), cli, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := cli.calls[0].args
	if i := slices.Index(args, "--file"); i < 0 || args[i+1] != "Dockerfile" || filepath.IsAbs(args[i+1]) {
		t.Errorf("build args %q, want the context-relative Dockerfile", args)
	}
	if i := slices.Index(args, "--platform"); i < 0 || args[i+1] != "linux/arm64" || built.Platform != "linux/arm64" {
		t.Errorf("build args %q and receipt platform %q, want linux/arm64", args, built.Platform)
	}
	dir := filepath.Join(t.TempDir(), "cc-remote-image-nested")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := image.Write(dir); err != nil {
		t.Fatal(err)
	}
	rooted := &fakeNamespace{t: t}
	if err := rooted.Run(t.Context(), nsc, image.BuildArgs(dir, "")...); err != nil || rooted.files["--file"] != string(image.Dockerfile) {
		t.Errorf("the context-relative Dockerfile did not resolve under %s: %v", dir, err)
	}
	if err := rooted.Run(t.Context(), nsc, "build", dir, "--file", filepath.Join(dir, "Dockerfile")); err == nil {
		t.Error("a host-absolute Dockerfile path resolved under the context root")
	}
}
