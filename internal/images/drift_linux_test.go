package images

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestCaptainHookDriftRunsTheInterpreterOfTheBuildTheHostReports(t *testing.T) {
	home := t.TempDir()
	for build, hook := range map[string]string{"1.0.0": "hooks.merge_guard", "0.9.0": "hooks.stale"} {
		venv := filepath.Join(home, ".daemonkit", "tools", "capt-hook", build, "capt-hook", "bin")
		tool := filepath.Join(home, ".daemonkit", "tools", "capt-hook", build, "bin")
		for _, dir := range []string{venv, tool} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		python := "#!/bin/sh\n[ \"$1 $2\" = '-P -c' ] || exit 2\nprintf '{\"hook\":\"" + hook + "\",\"error\":\"ImportError: under %s\"}\\n' \"$4\"\n"
		if err := os.WriteFile(filepath.Join(venv, "python"), []byte(python), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(venv, "hook"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(venv, "hook"), filepath.Join(tool, "hook")); err != nil {
			t.Fatal(err)
		}
	}
	host := filepath.Join(home, ".local", "share", "captain-hook", "host")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "version.json"), []byte(`{"schema":1,"build":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := func(ctx context.Context, argv []string, _ io.Reader) ([]byte, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %s", err, stderr.String())
		}
		return out, nil
	}
	drift, err := Scripts{}.CaptainHookDrift(t.Context(), capture, "/workspaces/app")
	if err != nil {
		t.Fatal(err)
	}
	if want := []HookDrift{{Hook: "hooks.merge_guard", Error: "ImportError: under /workspaces/app"}}; !slices.Equal(drift, want) {
		t.Errorf("drift = %q, want %q", drift, want)
	}
}
