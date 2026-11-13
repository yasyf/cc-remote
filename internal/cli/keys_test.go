package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/state"
)

func TestCaptureEnvReusesItsLineWithinTheReuseWindow(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "env")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho run >> '"+runs+"'\nprintf 'CI_DOOR_URL=https://door.example/\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	states := state.Dir(filepath.Join(dir, "state"))
	for range 2 {
		line, err := captureEnv(t.Context(), states, []string{script})
		if err != nil || string(line) != "CI_DOOR_URL=https://door.example/" {
			t.Fatalf("captureEnv = %q, %v", line, err)
		}
	}
	raw, err := os.ReadFile(runs)
	if err != nil || strings.Count(string(raw), "run") != 1 {
		t.Errorf("the env command ran %q, %v; want once", raw, err)
	}
	files, err := filepath.Glob(filepath.Join(string(states), "keys", "env-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("env files = %v, %v", files, err)
	}
	if info, err := os.Stat(files[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("env file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestKeyOutputAnswersNoAWSFormItCannotParseInProcess(t *testing.T) {
	out, err := keyOutput(t.Context(), state.Dir(t.TempDir()), []string{"sh", "-c", "printf 'aws ssm get-parameter --name /x --query Parameter.Value --output text\\n'"})
	if err != nil || string(out) != "aws ssm get-parameter --name /x --query Parameter.Value --output text\n" {
		t.Errorf("keyOutput = %q, %v", out, err)
	}
}
