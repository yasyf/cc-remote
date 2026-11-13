package images

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginsMoveGoCachesOutOfTheHomeOnASprite(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	inv := validInventory()
	tmpl, err := parse(inv)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := execute(tmpl, "plugins.sh", newView(inv, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(rendered)
	start, end := strings.Index(script, "\nhome_go_cache() {\n"), strings.Index(script, "\nclaude_env() {\n")
	if start < 0 || end < start {
		t.Fatalf("plugins.sh has no home_go_cache and go_caches before claude_env:\n%s", script)
	}
	root := t.TempDir()
	home, bin, cache := filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "var-tmp")
	module := filepath.Join(root, "proxy", "example.com", "cached", "@v")
	for _, dir := range []string{home, bin, module} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "sprite-env"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const goMod, source = "module example.com/cached\n\ngo 1.21\n", "package cached\n\nconst Intact = \"every byte\"\n"
	for name, body := range map[string]string{"list": "v1.0.0\n", "v1.0.0.info": `{"Version":"v1.0.0"}`, "v1.0.0.mod": goMod} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := os.Create(filepath.Join(module, "v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	files := zip.NewWriter(archive)
	for name, body := range map[string]string{"go.mod": goMod, "cached.go": source} {
		w, err := files.Create("example.com/cached@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("bash", "-c", "set -euo pipefail\nroot_cache_dir="+quote(cache)+"\n"+script[start:end]+"\ngo_caches\ngo mod download example.com/cached@v1.0.0\ngo env GOMODCACHE GOCACHE GOTMPDIR\n")
	run.Dir = root
	run.Env = []string{
		"HOME=" + home,
		"PATH=" + bin + ":" + filepath.Dir(gobin) + ":/usr/bin:/bin",
		"GOENV=" + filepath.Join(root, "goenv"),
		"GOTOOLCHAIN=local",
		"GOPROXY=file://" + filepath.Join(root, "proxy"),
		"GOSUMDB=off",
		"GOFLAGS=-modcacherw",
	}
	var stderr bytes.Buffer
	run.Stderr = &stderr
	out, err := run.Output()
	if err != nil {
		t.Fatalf("go_caches and a module download: %v: %s", err, stderr.String())
	}
	if got, want := string(out), strings.Join([]string{cache + "/go-mod", cache + "/go-build", cache + "/go-tmp", ""}, "\n"); got != want {
		t.Errorf("go env = %q, want %q", got, want)
	}
	if got, err := os.ReadFile(filepath.Join(cache, "go-mod", "example.com", "cached@v1.0.0", "cached.go")); err != nil || string(got) != source {
		t.Errorf("the downloaded module's cached.go reads %q, %v; want %q", got, err, source)
	}
	if _, err := os.Stat(filepath.Join(home, "go", "pkg", "mod")); !os.IsNotExist(err) {
		t.Errorf("the download still wrote the home's module cache: %v", err)
	}
}
