package images

import (
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	assets "github.com/yasyf/cc-remote/images"
)

const (
	canonicalCodexConfig = "# synthetic opaque fixture bytes\nmodel = \"fixture-model\"\n[features]\nhooks = true\n\n[mcp_servers.fixture]\ncommand = \"fixture\"\n"
	canonicalCodexHooks  = "{\"synthetic\":\"opaque fixture bytes\",\"hooks\":[{\"event\":\"fixture\",\"command\":[\"fixture-hook\"]}]}\n"
)

func writeFinalize(t *testing.T, dir string) string {
	t.Helper()
	data, err := assets.FS.ReadFile("namespace/finalize.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "finalize.sh")
	writePluginTestFile(t, path, data, 0o755)
	return path
}

func listen(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func runtimeState(t *testing.T, home string) []string {
	t.Helper()
	var found []string
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSocket != 0 || strings.HasSuffix(path, ".pid") {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestBakeMaterializesNativesWithoutStartingServices(t *testing.T) {
	f := nativeHost(t, nativeOptions{})
	finalize := writeFinalize(t, f.h.fakes)
	for _, phase := range []string{"install", "natives"} {
		if out, err := f.h.plugins(phase); err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
	}
	if out, err := f.h.run("bash", finalize, "home", f.h.home); err != nil {
		t.Fatalf("finalize: %v\n%s", err, out)
	}
	root := nativeRoot(f.h.home, f.digest)
	for _, rel := range []string{"tool", "LICENSE", "meta.json"} {
		if info, err := os.Lstat(filepath.Join(root, rel)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the baked native cache lacks %s: %v", rel, err)
		}
	}
	runner := filepath.Join(f.h.home, ".daemonkit/binrun", nativeRunnerTag, "binrun")
	if info, err := os.Stat(runner); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the baked runner is %v, %v; want an executable", info, err)
	}
	for _, rel := range []string{".cc-remote/ready", ".cc-remote/services", ".cc-remote/supervise.py", ".cc-remote/start.sh", ".daemonkit/a"} {
		if _, err := os.Lstat(filepath.Join(f.h.home, rel)); !os.IsNotExist(err) {
			t.Errorf("the bake left %s: %v", rel, err)
		}
	}
	if state := runtimeState(t, f.h.home); len(state) != 0 {
		t.Errorf("the bake left sockets or PID files: %v", state)
	}
	for _, call := range f.h.calls() {
		if strings.Contains(call, "supervise") || strings.Contains(call, "sprite-env") || strings.Contains(call, "nohup") {
			t.Errorf("the bake started a service: %s", call)
		}
	}
	if err := os.RemoveAll(filepath.Join(f.h.fakes, "assets")); err != nil {
		t.Fatal(err)
	}
	writePluginTestFile(t, filepath.Join(f.h.fakes, "curl"), []byte("#!/bin/sh\necho \"curl $*\" >> \"$FAKE_LOG\"\nexit 22\n"), 0o755)
	downloads := len(curlCalls(f.h))
	descriptor := filepath.Join(f.h.home, nativeCache, "bin/tool.binrun")
	if out, err := f.h.run(runner, "--", "fetch", descriptor); err != nil {
		t.Fatalf("the baked launcher's runner could not resolve its native offline: %v\n%s", err, out)
	}
	if got := len(curlCalls(f.h)); got != downloads {
		t.Errorf("resolving the baked native downloaded %d times", got-downloads)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.run(runner, "--", "fetch", descriptor); err == nil {
		t.Error("an unbaked native resolved with downloads failing, so the offline check proves nothing")
	}
}

func TestFinalizeExcludesInstallerStateAndKeepsReusableContent(t *testing.T) {
	home, err := os.MkdirTemp("", "ccr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	const identity = "synthetic-anonymous-0000"
	excluded := map[string]string{
		".claude.json":                                         `{"userID":"` + identity + `","anonymousId":"` + identity + `","oauthAccount":{"accountUuid":"` + identity + `"},"projects":{"/workspaces/app":{"lastSessionId":"` + identity + `"}},"cachedStatsigGates":{},"cachedGrowthBookFeatures":{},"firstStartTime":"2026-01-01T00:00:00Z","numStartups":1}`,
		".claude.json.backup":                                  identity,
		".claude/.credentials.json":                            `{"claudeAiOauth":{"accessToken":"synthetic"}}`,
		".claude/projects/-workspaces-app/session.jsonl":       identity,
		".claude/todos/" + identity + ".json":                  "[]",
		".claude/shell-snapshots/snapshot.sh":                  "true\n",
		".claude/statsig/statsig.stable_id":                    identity,
		".claude/session-env/" + identity + "/env":             "A=1\n",
		".claude/debug/latest":                                 identity,
		".claude/ide/1.lock":                                   "{}",
		".claude/history.jsonl":                                identity,
		".claude/backups/.claude.json.backup.1790998918598":    `{"userID":"` + identity + `","anonymousId":"` + identity + `"}`,
		".claude/plugins/data/hook/state.json":                 identity,
		".claude/state/hooks/grants.db":                        identity,
		".claude/state/hooks/grants.db-wal":                    identity,
		".claude/state/hooks/grants.db-shm":                    identity,
		".claude/plugins/cache/market/hook/1.0.0/.in_use":      identity,
		".claude/plugins/cache/market/hook/0.9.0/.orphaned_at": "1",
		".codex/auth.json":                                     `{"OPENAI_API_KEY":"synthetic"}`,
		".codex/sessions/2026/01/01/rollout.jsonl":             identity,
		".codex/log/codex-tui.log":                             identity,
		".codex/history.jsonl":                                 identity,
		".daemonkit/a/com.example.hook/registration.json":      identity,
		".daemonkit/run/hook.pid":                              "123",
		".daemonkit/cache/ab/ab12.lock":                        "",
		".cc-remote/ready":                                     "stamp",
		".cc-remote/services/hook":                             "exec hook",
		".cc-remote/supervise.py":                              "",
		".cc-remote/start.sh":                                  "",
		".cc-remote/tailscaled.state":                          identity,
		".cc-remote/tailscaled.log":                            identity,
		".cc-remote/orca/serve.json":                           identity,
		".config/gh/hosts.yml":                                 "github.com:\n  oauth_token: synthetic\n",
		".git-credentials":                                     "https://x:synthetic@github.com\n",
	}
	kept := map[string]string{
		".claude/settings.json":                                                      `{"enabledPlugins":{"hook@market":true}}`,
		".claude/plugins/installed_plugins.json":                                     `{"version":2}`,
		".claude/plugins/known_marketplaces.json":                                    `{}`,
		".claude/plugins/cache/market/hook/1.0.0/bin/hook":                           "#!/bin/sh\n",
		".claude/plugins/marketplaces/official/README.md":                            "readme",
		".local/share/cc-remote/marketplaces/market/.claude-plugin/marketplace.json": "{}",
		".local/share/cc-remote/tools/jq-1.8.2/jq":                                   "jq",
		".daemonkit/binrun/v0.8.0/binrun":                                            "runner",
		".daemonkit/cache/ab/" + digest + "/tool":                                    "tool",
		".daemonkit/cache/ab/" + digest + "/meta.json":                               `{"name":"tool"}`,
		".daemonkit/tools/capt-hook/12.79.8/capt-hookd":                              "hook",
		".codex/config.toml":                                                         canonicalCodexConfig,
		".codex/hooks.json":                                                          canonicalCodexHooks,
		".codex/plugins/cache/openai-primary-runtime/pdf/.codex-plugin/plugin.json":  "{}",
		".cache/uv/archive-v0/pkg":                                                   "cache",
		".cc-remote/plugins.sh":                                                      "#!/bin/bash\n",
		".agent-browser/browsers/chrome-154.0.8037.92/chrome-linux64/chrome":         "chrome",
		".local/share/cc-remote/tools/orca-runtime-1.4.218/squashfs-root/AppRun":     "#!/bin/sh\n",
	}
	for rel, data := range excluded {
		writePluginTestFile(t, filepath.Join(home, rel), []byte(data), 0o600)
	}
	for rel, data := range kept {
		writePluginTestFile(t, filepath.Join(home, rel), []byte(data), 0o644)
	}
	if err := os.Symlink(filepath.Join(home, ".daemonkit/cache"), filepath.Join(home, ".claude/backups/native-cache")); err != nil {
		t.Fatal(err)
	}
	listen(t, filepath.Join(home, ".daemonkit/a/com.example.hook/sv.sock"))
	listen(t, filepath.Join(home, ".local/state/agent.sock"))
	roots := []string{
		".claude.json", ".claude/backups", ".claude/.credentials.json", ".claude/projects", ".claude/todos", ".claude/shell-snapshots",
		".claude/statsig", ".claude/session-env", ".claude/debug", ".claude/ide", ".claude/history.jsonl", ".claude/plugins/data", ".claude/state",
		".codex/auth.json", ".codex/sessions", ".codex/log", ".codex/history.jsonl", ".daemonkit/a", ".cc-remote/ready",
		".cc-remote/services", ".cc-remote/start.sh", ".cc-remote/supervise.py", ".cc-remote/orca", ".config/gh/hosts.yml", ".git-credentials",
	}
	for _, rel := range roots {
		if _, err := os.Lstat(filepath.Join(home, rel)); err != nil {
			t.Fatalf("the fixture lacks %s: %v", rel, err)
		}
	}
	finalize := writeFinalize(t, t.TempDir())
	for range 2 {
		if out, err := exec.Command("bash", finalize, "home", home).CombinedOutput(); err != nil {
			t.Fatalf("finalize: %v\n%s", err, out)
		}
	}
	for rel := range excluded {
		if _, err := os.Lstat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("finalization kept %s: %v", rel, err)
		}
	}
	for _, rel := range roots {
		if _, err := os.Lstat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("finalization kept the excluded path %s: %v", rel, err)
		}
	}
	for rel, want := range kept {
		if got, err := os.ReadFile(filepath.Join(home, rel)); err != nil || string(got) != want {
			t.Errorf("finalization changed %s: %q, %v", rel, got, err)
		}
	}
	if state := runtimeState(t, home); len(state) != 0 {
		t.Errorf("finalization kept sockets or PID files: %v", state)
	}
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data)+path, identity) {
			t.Errorf("the shared identifier survives in %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeKeepsCanonicalCodexHooksAndToolConfiguration(t *testing.T) {
	finalize := writeFinalize(t, t.TempDir())
	seed := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		for rel, data := range map[string]string{
			".codex/hooks.json":                        canonicalCodexHooks,
			".codex/config.toml":                       canonicalCodexConfig,
			".claude/settings.json":                    `{"hooks":{"fixture":[]}}`,
			".codex/auth.json":                         `{"tokens":{"id_token":"synthetic"}}`,
			".codex/sessions/2026/01/01/rollout.jsonl": "synthetic",
			".codex/log/codex-tui.log":                 "synthetic",
			".codex/history.jsonl":                     "synthetic",
			".claude.json":                             `{"userID":"synthetic"}`,
		} {
			writePluginTestFile(t, filepath.Join(home, rel), []byte(data), 0o600)
		}
		return home
	}
	t.Run("kept byte for byte", func(t *testing.T) {
		home := seed(t)
		if out, err := exec.Command("bash", finalize, "home", home).CombinedOutput(); err != nil {
			t.Fatalf("finalize: %v\n%s", err, out)
		}
		for rel, want := range map[string]string{".codex/hooks.json": canonicalCodexHooks, ".codex/config.toml": canonicalCodexConfig, ".claude/settings.json": `{"hooks":{"fixture":[]}}`} {
			if got, err := os.ReadFile(filepath.Join(home, rel)); err != nil || string(got) != want {
				t.Errorf("%s = %q, %v; want the canonical bytes", rel, got, err)
			}
		}
		for _, rel := range []string{".codex/auth.json", ".codex/sessions", ".codex/log", ".codex/history.jsonl", ".claude.json"} {
			if _, err := os.Lstat(filepath.Join(home, rel)); !os.IsNotExist(err) {
				t.Errorf("finalization kept %s: %v", rel, err)
			}
		}
	})
	t.Run("lost configuration fails the build", func(t *testing.T) {
		home := seed(t)
		hooks := filepath.Join(home, ".codex/hooks.json")
		session := filepath.Join(home, ".codex/sessions/hooks.json")
		writePluginTestFile(t, session, []byte(canonicalCodexHooks), 0o600)
		if err := os.Remove(hooks); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(session, hooks); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", finalize, "home", home).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "changed the tool configuration "+hooks) {
			t.Errorf("finalize = %v\n%s; want a refusal for the lost hooks", err, out)
		}
	})
}

func TestFirstLayerHostKeyRemovalKeepsSSHConfiguration(t *testing.T) {
	root := t.TempDir()
	keys := []string{"ssh_host_rsa_key", "ssh_host_rsa_key.pub", "ssh_host_ecdsa_key", "ssh_host_ecdsa_key.pub", "ssh_host_ed25519_key", "ssh_host_ed25519_key.pub"}
	for _, key := range keys {
		writePluginTestFile(t, filepath.Join(root, "etc/ssh", key), []byte("synthetic "+key+"\n"), 0o600)
	}
	config := map[string]string{
		"etc/ssh/sshd_config":                   "Include /etc/ssh/sshd_config.d/*.conf\nKbdInteractiveAuthentication no\n",
		"etc/ssh/sshd_config.d/50-fixture.conf": "PasswordAuthentication no\n",
		"etc/ssh/ssh_config":                    "Host *\n  SendEnv LANG\n",
		"etc/ssh/moduli":                        "synthetic moduli\n",
		"etc/ssh/ssh_import_id":                 "{}\n",
	}
	for rel, data := range config {
		writePluginTestFile(t, filepath.Join(root, rel), []byte(data), 0o644)
	}
	finalize := writeFinalize(t, t.TempDir())
	out, err := exec.Command("bash", finalize, "system", root).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "kept SSH host key "+filepath.Join(root, "etc/ssh/ssh_host_")) {
		t.Fatalf("the check accepted remaining host keys: %v\n%s", err, out)
	}
	image, err := RenderImage(validInventory(), "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(image.Dockerfile), "\n    && rm -f /etc/ssh/ssh_host_* \\\n    && bash /tmp/cc-remote/finalize.sh system /\n") {
		t.Fatalf("the image does not remove and check host keys at the end of its package RUN:\n%s", image.Dockerfile)
	}
	if out, err := exec.Command("sh", "-c", `rm -f "$1"/etc/ssh/ssh_host_*`, "sh", root).CombinedOutput(); err != nil {
		t.Fatalf("the bounded removal: %v\n%s", err, out)
	}
	if out, err := exec.Command("bash", finalize, "system", root).CombinedOutput(); err != nil {
		t.Fatalf("the check rejected a clean root: %v\n%s", err, out)
	}
	for _, key := range keys {
		if _, err := os.Lstat(filepath.Join(root, "etc/ssh", key)); !os.IsNotExist(err) {
			t.Errorf("the removal kept %s: %v", key, err)
		}
	}
	for rel, want := range config {
		if got, err := os.ReadFile(filepath.Join(root, rel)); err != nil || string(got) != want {
			t.Errorf("the removal changed %s: %q, %v", rel, got, err)
		}
	}
	writePluginTestFile(t, filepath.Join(root, "etc/ssh/ssh_host_ed25519_key"), []byte("synthetic\n"), 0o600)
	if out, err := exec.Command("bash", finalize, "system", root).CombinedOutput(); err == nil || !strings.Contains(string(out), "ssh_host_ed25519_key") {
		t.Errorf("the check accepted a remaining private key: %v\n%s", err, out)
	}
}
