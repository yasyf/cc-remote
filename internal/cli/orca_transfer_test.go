package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
)

const (
	fakeKeyDir   = "/home/agent/.cc-remote/orca/key.Ab12"
	secretPatch  = "SECRET-PATCH-BYTES"
	secretReport = "SECRET-REPORT-BYTES"
	secretStderr = "SECRET-STDERR sk-synthetic-credential"
)

func fakeRemote(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
for arg do
  if [ "$arg" = check ]; then printf 'check\0' >> "$SSH_LOG"; exit 0; fi
done
for script do :; done
printf '%s\0' "$script" >> "$SSH_LOG"
case $script in
*mkfifo*) printf '%s\n' ` + fakeKeyDir + `; exit 0 ;;
*key.Ab12/key*) cat > /dev/null; exit 0 ;;
*"rm -rf"*) exit 0 ;;
esac
if [ -n "${REMOTE_FAIL-}" ]; then
  case $script in *"$REMOTE_FAIL"*) printf '%s\n' '` + secretStderr + `' >&2; printf '%s\n' '` + secretStderr + `'; exit 7 ;; esac
fi
export HOME="$REMOTE_HOME"
cd "$HOME"
status=0
sh -c "sh -c $script" || status=$?
case $script in *brief.md*) if [ -n "${REMOTE_CORRUPT-}" ]; then printf x; fi ;; esac
exit "$status"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SSH_LOG", filepath.Join(t.TempDir(), "ssh.log"))
	t.Setenv("REMOTE_HOME", home)
	return home
}

func sshCalls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("SSH_LOG"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, script := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		kind := "other: " + script
		switch {
		case script == "check":
			kind = "check"
		case strings.Contains(script, "mkfifo"):
			kind = "key-dir"
		case strings.Contains(script, "key.Ab12/key"):
			kind = "key-write"
		case strings.Contains(script, "rev-parse --verify HEAD"):
			kind = "head"
		case strings.Contains(script, "brief.md") && strings.Contains(script, "mkdir"):
			kind = "brief"
		case strings.Contains(script, `exec cat "$file"`):
			kind = "artifact"
		case strings.Contains(script, "status --porcelain=v1 -z"):
			kind = "status"
		}
		calls = append(calls, kind)
	}
	return calls
}

func gitCheckout(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitOutput(t, root, "init", "-q")
	for name, body := range map[string]string{"README": "app\n", "old.txt": "old\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitOutput(t, root, "add", ".")
	gitOutput(t, root, "commit", "-q", "-m", "init")
	return root, gitOutput(t, root, "rev-parse", "HEAD")
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReadBriefKeepsEveryByte(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	body := "  read 'this' \"brief\" $HOME `id`\n\n\ttrailing whitespace \n\n"
	if got, err := readBrief(write("brief.md", body)); err != nil || string(got) != body {
		t.Errorf("readBrief = %q, %v", got, err)
	}
	for _, empty := range []string{"", " \n\t\r\n"} {
		if _, err := readBrief(write("empty.md", empty)); err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Errorf("readBrief(%q) = %v", empty, err)
		}
	}
	if _, err := readBrief(filepath.Join(dir, "absent.md")); err == nil || !strings.Contains(err.Error(), "read the brief") {
		t.Errorf("readBrief of a missing file = %v", err)
	}
}

func TestPublishCopiesTheExactBriefOutsideTheCheckout(t *testing.T) {
	home := fakeRemote(t)
	root, head := gitCheckout(t)
	brief := []byte("# Task\n\n\"double\" 'single' $(touch pwned) `touch pwned` ; & | > out \\ *\x00\ttrailing  \n\n")
	driver := orcaDriver{state: state.Dir(t.TempDir())}
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "task-a", Machine: "task-a", ProjectRoot: root, Forward: &orcaTunnel{Host: "task-a"}, Terminal: "term-1"}
	if err := driver.publish(t.Context(), task, brief); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a")
	want := artifactOf(dir+"/brief.md", brief)
	if !task.Prepared || task.Brief == nil || *task.Brief != want || task.BaseCommit != head || len(task.Receipts) != 0 {
		t.Fatalf("task = %+v, brief %+v, want %+v at %s", task, task.Brief, want, head)
	}
	if copied, err := os.ReadFile(want.Path); err != nil || !bytes.Equal(copied, brief) {
		t.Errorf("the VM copy = %q, %v", copied, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("task directory = %v, %v", info, err)
	}
	if info, err := os.Stat(want.Path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("brief file = %v, %v", info, err)
	}
	for _, place := range []string{home, root, dir} {
		if _, err := os.Stat(filepath.Join(place, "pwned")); !os.IsNotExist(err) {
			t.Errorf("the brief ran as shell in %s: %v", place, err)
		}
	}
	if got := gitOutput(t, root, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout changed: %q", got)
	}
	loaded, err := loadTask(driver.state, "task-a")
	if err != nil || !loaded.Prepared || *loaded.Brief != want || loaded.BaseCommit != head || loaded.Receipts != nil {
		t.Fatalf("stored task = %+v, %v", loaded, err)
	}
	raw, err := os.ReadFile(driver.state.Orca("task-a"))
	if err != nil || bytes.Contains(raw, []byte("receipts")) || bytes.Contains(raw, []byte("pwned")) {
		t.Errorf("stored record = %s, %v", raw, err)
	}
	if got := sshCalls(t); !slices.Equal(got, []string{"head", "brief"}) {
		t.Errorf("ssh calls = %q", got)
	}
}

func TestPublishRefusesAnUnverifiedCopy(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, home, root string)
		want    string
		base    bool
		ssh     []string
	}{
		{"altered bytes", func(t *testing.T, _, _ string) { t.Setenv("REMOTE_CORRUPT", "1") }, "not the brief's 12 bytes", true, []string{"head", "brief"}},
		{"directory already there", func(t *testing.T, home, _ string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "copy the brief on task-a exited 1: its remote output is withheld", true, []string{"head", "brief"}},
		{"state linked into the checkout", func(t *testing.T, home, root string) {
			if err := os.Mkdir(filepath.Join(root, "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "state"), filepath.Join(home, ".cc-remote")); err != nil {
				t.Fatal(err)
			}
		}, "copy the brief on task-a exited 3: the task directory resolves inside the checkout", true, []string{"head", "brief"}},
		{"brief copy fails", func(t *testing.T, _, _ string) { t.Setenv("REMOTE_FAIL", "brief.md") }, "copy the brief on task-a exited 7: its remote output is withheld", true, []string{"head", "brief"}},
		{"HEAD read fails", func(t *testing.T, _, _ string) { t.Setenv("REMOTE_FAIL", "rev-parse") }, "read the checkout's HEAD on task-a exited 7: its remote output is withheld", false, []string{"head"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := fakeRemote(t)
			root, head := gitCheckout(t)
			tt.prepare(t, home, root)
			driver := orcaDriver{state: state.Dir(t.TempDir())}
			task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "task-a", Machine: "task-a", ProjectRoot: root, Forward: &orcaTunnel{Host: "task-a"}, Terminal: "term-1"}
			if err := driver.save(task); err != nil {
				t.Fatal(err)
			}
			err := driver.publish(t.Context(), task, []byte("BRIEF-BYTES\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "BRIEF-BYTES") || strings.Contains(err.Error(), secretStderr) {
				t.Fatalf("publish = %v, want %q", err, tt.want)
			}
			base := ""
			if tt.base {
				base = head
			}
			loaded, err := loadTask(driver.state, "task-a")
			if err != nil || loaded.Prepared || loaded.Brief != nil || loaded.BaseCommit != base || loaded.Terminal != "term-1" {
				t.Errorf("stored task = %+v, %v", loaded, err)
			}
			if _, err := os.Stat(filepath.Join(root, "state", "orca", "tasks", "task-a", briefName)); !os.IsNotExist(err) {
				t.Errorf("the brief reached the checkout: %v", err)
			}
			if got := gitOutput(t, root, "status", "--porcelain", "--untracked-files=all"); got != "" {
				t.Errorf("the checkout changed: %q", got)
			}
			if got := sshCalls(t); !slices.Equal(got, tt.ssh) {
				t.Errorf("ssh calls = %q, want %q", got, tt.ssh)
			}
		})
	}
}

type collectFixture struct {
	home   string
	config string
	root   string
	head   string
	tasks  string
	output string
	patch  []byte
}

func newCollectFixture(t *testing.T) collectFixture {
	t.Helper()
	home := fakeRemote(t)
	root, head := gitCheckout(t)
	tasks := filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a")
	if err := os.MkdirAll(tasks, 0o700); err != nil {
		t.Fatal(err)
	}
	brief := []byte("the brief\n")
	patch := []byte("diff --git a/README b/README\n+" + secretPatch + "\n\x00binary\n")
	files := map[string][]byte{"brief.md": brief, "change.patch": patch}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tasks, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("app\n"+secretPatch+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "old.txt")); err != nil {
		t.Fatal(err)
	}
	dir := state.Dir(t.TempDir())
	control, err := state.NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
	task := &orcaTask{
		SchemaVersion: orcaTaskSchema, Workspace: "task-a", Provider: "absent", Profile: "lean", Machine: "task-a", ProjectRoot: root,
		Port: 7001, Forward: &orcaTunnel{Host: "task-a", Config: "/recorded/config", Control: control, Log: dir.OrcaForwardLog("task-a")},
		Environment: "task-a", EnvironmentID: "env-1", RuntimeID: "rt-1", RepoID: "repo-1", WorktreeID: "repo-1::" + root,
		Agent: orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, Terminal: "term-1",
		Prepared: true, Brief: &orcaArtifact{Path: tasks + "/brief.md", SHA256: artifactOf("", brief).SHA256, Bytes: int64(len(brief))}, BaseCommit: head,
	}
	if err := state.Save(dir.Orca("task-a"), task); err != nil {
		t.Fatal(err)
	}
	f := collectFixture{home: home, config: taskConfig(t, dir), root: root, head: head, tasks: tasks, output: filepath.Join(t.TempDir(), "collected"), patch: patch}
	f.report(t, map[string]any{
		"schemaVersion": 1, "baseCommit": head,
		"patch": map[string]any{"sha256": artifactOf("", patch).SHA256, "bytes": len(patch)},
		"files": []map[string]any{
			{"path": "README", "sha256": artifactOf("", []byte("app\n"+secretPatch+"\n")).SHA256},
			{"path": "new.txt", "sha256": artifactOf("", []byte("new\n")).SHA256},
			{"path": "old.txt", "deleted": true},
		},
	})
	return f
}

func taskConfig(t *testing.T, dir state.Dir) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf("repository: https://github.com/example/app\nref: main\nprovider: absent\nprofile: lean\ninventory: missing.yaml\nproviders:\n  absent: {}\nworkspace_dirs:\n  absent: /home/agent\nprofiles:\n  lean: {}\nstate_dir: %q\n", dir)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f collectFixture) report(t *testing.T, report map[string]any) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.tasks, "report.json"), append(raw, "\n \t\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f collectFixture) remoteState(t *testing.T) string {
	t.Helper()
	var snapshot strings.Builder
	for _, dir := range []string{f.tasks, filepath.Join(f.root, ".git")} {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			target, _ := os.Readlink(path)
			raw, _ := os.ReadFile(path)
			fmt.Fprintf(&snapshot, "%s %v %d %s %s\n", path, info.Mode(), info.ModTime().UnixNano(), target, artifactOf("", raw).SHA256)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot.String() + gitOutput(t, f.root, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all")
}

func (f collectFixture) collect(t *testing.T, runtimeID string, args ...string) (string, int, error) {
	t.Helper()
	calls := 0
	runner := taskOrcaRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if !slices.Equal(args, []string{"status", "--environment", "task-a", "--json"}) {
			t.Errorf("unexpected runtime operation: %v", args)
		}
		return fmt.Appendf(nil, `{"ok":true,"result":{"runtime":{"reachable":true,"runtimeId":%q}},"_meta":{"runtimeId":%q}}`, runtimeID, runtimeID), nil
	})
	cmd := newOrcaCollectCmd(runner)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"task-a", "--config", f.config}, args...))
	err := cmd.ExecuteContext(t.Context())
	return out.String(), calls, err
}

func (f collectFixture) args() []string {
	return []string{"--report-file", f.tasks + "/report.json", "--patch-file", f.tasks + "/change.patch", "--output", f.output}
}

func TestCollectCopiesTheDeclaredArtifactsReadOnly(t *testing.T) {
	f := newCollectFixture(t)
	report, err := os.ReadFile(filepath.Join(f.tasks, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	before := f.remoteState(t)
	out, calls, err := f.collect(t, "rt-1", f.args()...)
	if err != nil || calls != 1 {
		t.Fatalf("collect = %v, calls = %d: %s", err, calls, out)
	}
	var got orcaCollection
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("collect printed %s: %v", out, err)
	}
	want := orcaCollection{
		Workspace:  "task-a",
		BaseCommit: f.head,
		Report:     artifactOf(filepath.Join(f.output, "report.json"), report),
		Patch:      artifactOf(filepath.Join(f.output, "change.patch"), f.patch),
	}
	if got.Workspace != want.Workspace || got.BaseCommit != want.BaseCommit || got.Report != want.Report || got.Patch != want.Patch {
		t.Errorf("collection =\n%+v\nwant\n%+v", got, want)
	}
	slices.SortFunc(got.Status.Changes, func(a, b orcaChange) int { return strings.Compare(a.Path, b.Path) })
	changes := []orcaChange{{Status: " M", Path: "README"}, {Status: "??", Path: "new.txt"}, {Status: " D", Path: "old.txt"}}
	if !slices.Equal(got.Status.Changes, changes) || got.Status.Overflow || got.Status.Bytes != len(" M README\x00 D old.txt\x00?? new.txt\x00") {
		t.Errorf("status = %+v", got.Status)
	}
	for name, body := range map[string][]byte{"report.json": report, "change.patch": f.patch} {
		if kept, err := os.ReadFile(filepath.Join(f.output, name)); err != nil || !bytes.Equal(kept, body) {
			t.Errorf("collected %s = %q, %v", name, kept, err)
		}
	}
	if info, err := os.Stat(f.output); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("output directory = %v, %v", info, err)
	}
	if strings.Contains(out, secretPatch) {
		t.Errorf("collect printed the patch: %s", out)
	}
	if after := f.remoteState(t); after != before {
		t.Errorf("the worker changed:\n%s\nwant\n%s", after, before)
	}
	if got := sshCalls(t); !slices.Equal(got, []string{"check", "head", "artifact", "artifact", "status"}) {
		t.Errorf("ssh calls = %q", got)
	}
}

func TestCollectRefusesWithoutPrintingContents(t *testing.T) {
	type setup struct {
		f    collectFixture
		args []string
	}
	appendReport := func(suffix string) func(t *testing.T, s *setup) {
		return func(t *testing.T, s *setup) {
			report, err := os.OpenFile(filepath.Join(s.f.tasks, "report.json"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := report.WriteString(suffix); err != nil {
				t.Fatal(err)
			}
			if err := report.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	failRemote := func(script string) func(t *testing.T, s *setup) {
		return func(t *testing.T, _ *setup) { t.Setenv("REMOTE_FAIL", script) }
	}
	pathCase := func(report, patch string) func(t *testing.T, s *setup) {
		return func(_ *testing.T, s *setup) {
			s.args = []string{"--report-file", strings.ReplaceAll(report, "TASKS", s.f.tasks), "--patch-file", strings.ReplaceAll(patch, "TASKS", s.f.tasks), "--output", s.f.output}
		}
	}
	reportCase := func(edit func(map[string]any)) func(t *testing.T, s *setup) {
		return func(t *testing.T, s *setup) {
			report := map[string]any{
				"schemaVersion": 1, "baseCommit": s.f.head,
				"patch": map[string]any{"sha256": artifactOf("", s.f.patch).SHA256, "bytes": len(s.f.patch)},
				"files": []map[string]any{{"path": "README", "sha256": artifactOf("", nil).SHA256}},
			}
			edit(report)
			s.f.report(t, report)
		}
	}
	none := []string(nil)
	reads := []string{"check", "head", "artifact", "artifact"}
	tests := []struct {
		name    string
		runtime string
		setup   func(t *testing.T, s *setup)
		want    string
		ssh     []string
		calls   int
	}{
		{"relative report", "rt-1", pathCase("report.json", "TASKS/change.patch"), "not an artifact path", none, 0},
		{"patch elsewhere", "rt-1", pathCase("TASKS/report.json", "/tmp/change.patch"), "not an artifact path", none, 0},
		{"dot-dot patch", "rt-1", pathCase("TASKS/report.json", "TASKS/../task-b/change.patch"), "not an artifact path", none, 0},
		{"the brief itself", "rt-1", pathCase("TASKS/brief.md", "TASKS/change.patch"), "not an artifact path", none, 0},
		{"one file twice", "rt-1", pathCase("TASKS/change.patch", "TASKS/change.patch"), "the same file", none, 0},
		{"existing output", "rt-1", func(t *testing.T, s *setup) {
			if err := os.Mkdir(s.f.output, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "must not exist yet", none, 0},
		{"not prepared", "rt-1", func(t *testing.T, s *setup) {
			task, err := loadTask(collectState(t, s.f), "task-a")
			if err != nil {
				t.Fatal(err)
			}
			task.Prepared = false
			if err := state.Save(collectState(t, s.f).Orca("task-a"), task); err != nil {
				t.Fatal(err)
			}
		}, "not a prepared task", none, 0},
		{"other runtime", "rt-2", func(*testing.T, *setup) {}, `answered from runtime "rt-2", not rt-1`, []string{"check"}, 1},
		{"moved checkout", "rt-1", func(t *testing.T, s *setup) {
			gitOutput(t, s.f.root, "commit", "-q", "--allow-empty", "-m", "moved")
		}, "not the prepared baseCommit", []string{"check", "head"}, 1},
		{"missing patch", "rt-1", func(t *testing.T, s *setup) {
			if err := os.Remove(filepath.Join(s.f.tasks, "change.patch")); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file", []string{"check", "head", "artifact", "artifact"}, 1},
		{"linked patch", "rt-1", func(t *testing.T, s *setup) {
			outside := filepath.Join(t.TempDir(), "outside.patch")
			if err := os.WriteFile(outside, []byte(secretPatch), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(s.f.tasks, "change.patch")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(s.f.tasks, "change.patch")); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file", []string{"check", "head", "artifact", "artifact"}, 1},
		{"other base", "rt-1", reportCase(func(r map[string]any) { r["baseCommit"] = strings.Repeat("a", 40) }), "baseCommit is not the prepared", reads, 1},
		{"other digest", "rt-1", reportCase(func(r map[string]any) {
			r["patch"].(map[string]any)["sha256"] = artifactOf("", []byte(secretReport)).SHA256
		}), "another digest or length", reads, 1},
		{"other length", "rt-1", reportCase(func(r map[string]any) {
			patch := r["patch"].(map[string]any)
			patch["bytes"] = patch["bytes"].(int) + 1
		}), "another digest or length", reads, 1},
		{"other schema", "rt-1", reportCase(func(r map[string]any) { r["schemaVersion"] = 2 }), "not a schema 1 collection report", reads, 1},
		{"unknown field", "rt-1", reportCase(func(r map[string]any) { r["note"] = secretReport }), "not a schema 1 collection report", reads, 1},
		{"not JSON", "rt-1", func(t *testing.T, s *setup) {
			if err := os.WriteFile(filepath.Join(s.f.tasks, "report.json"), []byte(secretReport), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a schema 1 collection report", reads, 1},
		{"hash and deletion", "rt-1", reportCase(func(r map[string]any) {
			r["files"] = []map[string]any{{"path": "README", "sha256": artifactOf("", nil).SHA256, "deleted": true}}
		}), "files[0]", reads, 1},
		{"neither hash nor deletion", "rt-1", reportCase(func(r map[string]any) {
			r["files"] = []map[string]any{{"path": "README"}}
		}), "files[0]", reads, 1},
		{"path outside the checkout", "rt-1", reportCase(func(r map[string]any) {
			r["files"] = []map[string]any{{"path": "../" + secretReport, "deleted": true}}
		}), "files[0]", reads, 1},
		{"duplicate path", "rt-1", reportCase(func(r map[string]any) {
			r["files"] = []map[string]any{{"path": "old.txt", "deleted": true}, {"path": "old.txt", "deleted": true}}
		}), "files[1]", reads, 1},
		{"second report object", "rt-1", appendReport(`{"schemaVersion":1}`), "not exactly one JSON document", reads, 1},
		{"trailing garbage", "rt-1", appendReport(secretReport), "not exactly one JSON document", reads, 1},
		{"linked ancestor", "rt-1", func(t *testing.T, s *setup) {
			tasks, moved := filepath.Dir(s.f.tasks), filepath.Join(t.TempDir(), "tasks")
			if err := os.Rename(tasks, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, tasks); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file directly in the recorded physical task directory", []string{"check", "head", "artifact"}, 1},
		{"HEAD read fails", "rt-1", failRemote("rev-parse"), "read the checkout's HEAD on task-a exited 7: its remote output is withheld", []string{"check", "head"}, 1},
		{"artifact read fails", "rt-1", failRemote(`exec cat "$file"`), "on task-a exited 7: its remote output is withheld", []string{"check", "head", "artifact"}, 1},
		{"status read fails", "rt-1", failRemote("porcelain"), "read git status on task-a exited 7: its remote output is withheld", []string{"check", "head", "artifact", "artifact", "status"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &setup{f: newCollectFixture(t)}
			s.args = s.f.args()
			tt.setup(t, s)
			existed := fileExists(s.f.output)
			before := s.f.remoteState(t)
			out, calls, err := s.f.collect(t, tt.runtime, s.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) || calls != tt.calls {
				t.Fatalf("collect = %v, calls = %d, want %q and %d calls", err, calls, tt.want, tt.calls)
			}
			for _, secret := range []string{secretPatch, secretReport, secretStderr} {
				if strings.Contains(err.Error(), secret) || strings.Contains(out, secret) {
					t.Errorf("the failure printed artifact contents: %v\n%s", err, out)
				}
			}
			if fileExists(s.f.output) != existed {
				t.Errorf("collect changed the output %s", s.f.output)
			}
			if entries, _ := os.ReadDir(s.f.output); existed && len(entries) != 0 {
				t.Errorf("collect wrote into the existing output: %v", entries)
			}
			if after := s.f.remoteState(t); after != before {
				t.Errorf("the worker changed:\n%s\nwant\n%s", after, before)
			}
			if got := sshCalls(t); !slices.Equal(got, tt.ssh) {
				t.Errorf("ssh calls = %q, want %q", got, tt.ssh)
			}
		})
	}
}

func collectState(t *testing.T, f collectFixture) state.Dir {
	t.Helper()
	cfg, err := (&selection{config: f.config}).load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.State()
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestStatusIsBoundedAndMarksOverflow(t *testing.T) {
	raw := " M README\x00R  new.go\x00old.go\x00?? b.txt\x00"
	modified := orcaChange{Status: " M", Path: "README"}
	renamed := orcaChange{Status: "R ", Path: "new.go", From: "old.go"}
	untracked := orcaChange{Status: "??", Path: "b.txt"}
	tests := []struct {
		name     string
		limit    int
		want     []orcaChange
		overflow bool
	}{
		{"complete", len(raw), []orcaChange{modified, renamed, untracked}, false},
		{"empty", 0, []orcaChange{}, true},
		{"cut inside an entry", len(" M README\x00R  ne"), []orcaChange{modified}, true},
		{"cut before a rename's origin", len(" M README\x00R  new.go\x00"), []orcaChange{modified}, true},
		{"cut inside a rename's origin", len(" M README\x00R  new.go\x00ol"), []orcaChange{modified}, true},
		{"cut after a rename", len(" M README\x00R  new.go\x00old.go\x00"), []orcaChange{modified, renamed}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStatus([]byte(raw)[:tt.limit], len(raw))
			if !slices.Equal(got.Changes, tt.want) || got.Overflow != tt.overflow || got.Bytes != len(raw) {
				t.Errorf("parseStatus = %+v", got)
			}
		})
	}
	if got := parseStatus(nil, 0); got.Overflow || got.Changes == nil || len(got.Changes) != 0 {
		t.Errorf("a clean checkout = %+v", got)
	}
}
