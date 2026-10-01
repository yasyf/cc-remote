package workspace_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/workspace"
	"github.com/yasyf/cc-remote/internal/workspace/workspacetest"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runScript(t *testing.T, home, script, stdin string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "TMPDIR="+t.TempDir())
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.CombinedOutput()
}

func TestCheckoutScriptClonesShallowThenSwitchesRefWithoutKeepingTheToken(t *testing.T) {
	repository := workspacetest.GitRepository(t)
	origin := strings.TrimPrefix(repository, "file://")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "second")
	home := t.TempDir()
	root := filepath.Join(home, "app")
	if out, err := runScript(t, home, workspace.CheckoutScript(root, repository, "main", true), workspacetest.Token+"\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if git(t, root, "rev-parse", "HEAD") != git(t, origin, "rev-parse", "main") || git(t, root, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Error("the clone is not at the requested ref")
	}
	if shallow := git(t, root, "rev-parse", "--is-shallow-repository"); shallow != "true" {
		t.Errorf("a lean checkout is not shallow: %s", shallow)
	}
	if out, err := runScript(t, home, workspace.CheckoutScript(root, repository, "feature", true), workspacetest.Token+"\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if git(t, root, "rev-parse", "--abbrev-ref", "HEAD") != "feature" || git(t, root, "rev-parse", "HEAD") != git(t, origin, "rev-parse", "feature") {
		t.Error("a second checkout did not switch to the requested ref")
	}
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "third")
	if out, err := runScript(t, home, workspace.CheckoutScript(root, repository, "main", false), "\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if git(t, root, "rev-parse", "HEAD") != git(t, origin, "rev-parse", "main") {
		t.Error("a checkout with no token did not fast-forward")
	}
	out, err := exec.Command("grep", "-rl", workspacetest.Token, home).Output()
	if err == nil {
		t.Errorf("the token is on disk in %s", out)
	}
	if config := git(t, root, "config", "--list"); strings.Contains(config, "token") || strings.Contains(config, "askpass") {
		t.Errorf("the checkout's config carries %q", config)
	}
}

func TestCheckoutScriptNeverPutsTheTokenInArgv(t *testing.T) {
	script := workspace.CheckoutScript("/home/x/app", "https://github.com/example/app", "main", true)
	if strings.Contains(script, "x-access-token:") || !strings.Contains(script, "IFS= read -r token") || !strings.Contains(script, "GIT_ASKPASS") || !strings.Contains(script, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("script = %s", script)
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("%v: %s", err, out)
	}
}

func TestWarmStepsRerunOnlyWhenTheirInputsChange(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	git(t, root, "init", "-q")
	runs := filepath.Join(t.TempDir(), "runs")
	commit := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, root, "add", ".")
		git(t, root, "commit", "-qm", name)
	}
	warm := func(inputs []string) int {
		t.Helper()
		if out, err := runScript(t, home, workspace.WarmScript(root, inputs, []string{"echo ran >> " + runs}), ""); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		raw, err := os.ReadFile(runs)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "ran")
	}
	inputs := []string{"yarn.lock", "*package.json"}
	commit("yarn.lock", "a\n")
	for _, step := range []struct {
		change string
		want   int
	}{
		{"", 1},
		{"README.md", 1},
		{"yarn.lock", 2},
		{"README.md", 2},
		{"api/package.json", 3},
	} {
		if step.change != "" {
			commit(step.change, step.change+fmt.Sprint(step.want))
		}
		if got := warm(inputs); got != step.want {
			t.Fatalf("after changing %q the warm steps had run %d times, want %d", step.change, got, step.want)
		}
	}
	if got := warm(nil); got != 4 {
		t.Errorf("warm steps with no inputs ran %d times in total, want every call to run them", got)
	}
}

func TestRefreshScriptExportsTheForwardPortsBeforeTheSteps(t *testing.T) {
	root := t.TempDir()
	out, err := runScript(t, t.TempDir(), workspace.RefreshScript(root, []string{"WEB_PORT=4321", "ODD=it's"}, []string{`printf '%s %s %s' "$WEB_PORT" "$ODD" "$(pwd)"`}), "")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "4321 it's "+root {
		t.Errorf("printed %q", out)
	}
}

func TestBootstrapScriptsInstallThenRunTheShippedScript(t *testing.T) {
	home := t.TempDir()
	if out, err := runScript(t, home, workspace.WriteBootstrapScript, "#!/bin/sh\nIFS= read -r token\nprintf 'ran %s %s' \"$WEB_PORT\" \"$token\" > \"$HOME/ran\"\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := runScript(t, home, workspace.BootstrapScript(home, []string{"WEB_PORT=7"}), "tok\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(home, "ran")); err != nil || string(got) != "ran 7 tok" {
		t.Errorf("the bootstrap ran %q, %v", got, err)
	}
}

func TestAllocateForwardsKeepsHeldPortsAndAllocatesTheRest(t *testing.T) {
	labels := []workspace.LabelledEnv{{Label: "a", Env: "A_PORT"}, {Label: "b", Env: "B_PORT"}}
	forwards, err := workspace.AllocateForwards(labels, []workspace.Forward{{Label: "b", Port: 40001}})
	if err != nil {
		t.Fatal(err)
	}
	if len(forwards) != 2 || forwards[0].Label != "a" || forwards[0].Port == 0 || forwards[0].Port == 40001 || forwards[1] != (workspace.Forward{Label: "b", Port: 40001}) {
		t.Errorf("forwards = %+v", forwards)
	}
	env := workspace.ForwardEnv(forwards, labels)
	if len(env) != 2 || env[0] != fmt.Sprintf("A_PORT=%d", forwards[0].Port) || env[1] != "B_PORT=40001" {
		t.Errorf("env = %v", env)
	}
}

func TestTokenGoesOnlyToGitHub(t *testing.T) {
	if !workspace.TokenAllowed("https://github.com/example/app") || workspace.TokenAllowed("https://gitlab.com/example/app") || workspace.TokenAllowed("file:///tmp/app") {
		t.Error("TokenAllowed")
	}
}
