package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repository = "https://github.com/example/app"
	helper     = "/.provider/git-credential-helper"
)

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return root
}

func runClean(t *testing.T, home, root, credentialHelper string, forbidden ...string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", CleanScript(root, repository, credentialHelper, forbidden))
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME=", "TMPDIR="+t.TempDir(), "LC_ALL=en_US.UTF-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func TestCleanScriptPassesASpareWithNoCredentials(t *testing.T) {
	if err := runClean(t, t.TempDir(), gitRepo(t, repository), ""); err != nil {
		t.Fatal(err)
	}
}

func TestCleanScriptRefusesEveryCredentialASpareCouldKeep(t *testing.T) {
	token := "gho_" + strings.Repeat("a", 36)
	write := func(home, path, body string) error {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, path)), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(home, path), []byte(body), 0o600)
	}
	tests := map[string]func(home, root string) error{
		"git credentials":         func(home, _ string) error { return write(home, ".git-credentials", "https://x:y@github.com\n") },
		"gh hosts":                func(home, _ string) error { return write(home, ".config/gh/hosts.yml", "github.com:\n") },
		"claude login":            func(home, _ string) error { return write(home, ".claude/.credentials.json", "{}") },
		"codex login":             func(home, _ string) error { return write(home, ".codex/auth.json", "{}") },
		"authorized key":          func(home, _ string) error { return write(home, ".ssh/authorized_keys", "ssh-ed25519 AAAA\n") },
		"private key":             func(home, _ string) error { return write(home, ".ssh/id_ed25519", "key") },
		"userspace tailnet state": func(home, _ string) error { return write(home, ".cc-remote/tailscaled.state", "{}") },
		"credential helper": func(_, root string) error {
			return exec.Command("git", "-C", root, "config", "credential.helper", "store").Run()
		},
		"token in the remote": func(_, root string) error {
			return exec.Command("git", "-C", root, "remote", "set-url", "origin", "https://x-access-token:"+token+"@github.com/example/app").Run()
		},
		"another origin": func(_, root string) error {
			return exec.Command("git", "-C", root, "remote", "set-url", "origin", "https://github.com/other/app").Run()
		},
		"token in a config":        func(home, _ string) error { return write(home, ".config/tool/state", "token: "+token+"\n") },
		"token in the state dir":   func(home, _ string) error { return write(home, ".cc-remote/log", token) },
		"token in the share dir":   func(home, _ string) error { return write(home, ".local/share/cc-remote/x", token) },
		"a pat in the claude json": func(home, _ string) error { return write(home, ".claude.json", "github_pat_"+strings.Repeat("b", 40)) },
	}
	for name, plant := range tests {
		t.Run(name, func(t *testing.T) {
			home, root := t.TempDir(), gitRepo(t, repository)
			if err := plant(home, root); err != nil {
				t.Fatal(err)
			}
			if err := runClean(t, home, root, ""); err == nil {
				t.Errorf("a spare holding %s passed", name)
			}
		})
	}
}

func TestCleanScriptRefusesConfiguredForbiddenPaths(t *testing.T) {
	home, root := t.TempDir(), gitRepo(t, repository)
	if err := runClean(t, home, root, "", "$HOME/.config/frontend"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "frontend"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runClean(t, home, root, "", "$HOME/.config/frontend"); err == nil {
		t.Error("a spare holding a forbidden path passed")
	}
}

func checkout(t *testing.T, home string, helpers ...string) string {
	t.Helper()
	root := gitRepo(t, repository+".git")
	args := make([][]string, 0, 1+len(helpers))
	args = append(args, []string{"credential.https://github.com.usehttppath", "true"})
	for _, helper := range helpers {
		args = append(args, []string{"credential.https://github.com.helper", helper})
	}
	for _, pair := range args {
		cmd := exec.Command("git", append([]string{"config", "--global", "--add"}, pair...)...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return root
}

func appendGitConfig(t *testing.T, home, section string) {
	t.Helper()
	config, err := os.OpenFile(filepath.Join(home, ".gitconfig"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.WriteString(section)
	if err := errors.Join(err, config.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanScriptAcceptsOnlyTheProvidersOwnCredentialHelper(t *testing.T) {
	home := t.TempDir()
	root := checkout(t, home, "", helper)
	if err := runClean(t, home, root, helper); err != nil {
		t.Errorf("a checkout with only the provider's helper failed: %v", err)
	}
	if err := runClean(t, home, root, ""); err == nil {
		t.Error("a provider without a helper accepted another provider's helper")
	}
	home = t.TempDir()
	root = checkout(t, home, "", helper, "store")
	if err := runClean(t, home, root, helper); err == nil {
		t.Error("a credential store beside the provider's helper passed")
	}
	const valueless, store = "[credential]\n\thelper\n", "[credential]\n\thelper = store\n"
	tests := map[string]struct {
		helpers []string
		section string
		files   map[string]string
	}{
		"a helper hidden behind a newline":              {helpers: []string{"\ncredential.usehttppath ; !f() { echo password=leaked; }; f"}},
		"a valueless helper paired with a two-line one": {helpers: []string{helper + "\ncredential.usehttppath true"}, section: valueless},
		"a valueless helper":                            {section: valueless},
		"a store behind a byte no locale decodes":       {section: "[credential \"https://a.test\"]\n\tusehttppath = \xff\n" + store},
		"a separator byte inside a valueless key":       {section: "[credential \"https://a.test.usehttppath\x01suffix\"]\n\thelper\n"},
		"a store in the XDG config":                     {files: map[string]string{".config/git/config": store}},
		"a store behind an include":                     {section: "[include]\n\tpath = extra.gitconfig\n", files: map[string]string{"extra.gitconfig": store}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			root := checkout(t, home, tt.helpers...)
			appendGitConfig(t, home, tt.section)
			for path, content := range tt.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(home, path)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runClean(t, home, root, helper); err == nil {
				t.Errorf("%s passed", name)
			}
		})
	}
}

func TestCleanScriptRefusesACredentialWithoutPrintingIt(t *testing.T) {
	const secret = "ghp_sentinelsecretvaluethatmustnotleak0000"
	for name, local := range map[string]bool{"global helper": false, "checkout config": true} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			root := checkout(t, home)
			leak := "!f() { echo password=" + secret + "; }; f"
			if local {
				if out, err := exec.Command("git", "-C", root, "config", "credential.helper", leak).CombinedOutput(); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			} else {
				appendGitConfig(t, home, "[credential]\n\thelper = "+leak+"\n")
			}
			err := runClean(t, home, root, helper)
			if err == nil {
				t.Fatalf("a %s carrying a token passed", name)
			}
			if strings.Contains(err.Error(), "sentinelsecret") {
				t.Errorf("refusing a %s printed its value: %v", name, err)
			}
		})
	}
}

func seedAgentIdentity(t *testing.T, home string) (forgotten, kept []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".claude", "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	forgotten = []string{".claude.json", ".claude.json.tmp.2955.e5ce", ".claude/backups/.claude.json.backup.1790795049089"}
	kept = []string{".claude/settings.json"}
	for _, name := range append(forgotten, kept...) {
		if err := os.WriteFile(filepath.Join(home, name), []byte("spare"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return forgotten, kept
}

func TestFreshScriptReplacesHostKeysAndForgetsAgentState(t *testing.T) {
	bin, home := t.TempDir(), t.TempDir()
	calls := filepath.Join(t.TempDir(), "sudo.log")
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	forgotten, kept := seedAgentIdentity(t, home)
	cmd := exec.Command("sh", "-c", FreshScript(true))
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, name := range forgotten {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s, which carries the spare's agent identity, survived the claim: %v", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Errorf("the claim removed %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "-n rm -f /etc/ssh/ssh_host_") || lines[1] != "-n ssh-keygen -A" {
		t.Errorf("sudo ran %q; want the old host keys removed, then new ones generated, never a password prompt", lines)
	}
}

func TestFreshScriptForgetsTheAgentWithoutSudo(t *testing.T) {
	bin, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	forgotten, kept := seedAgentIdentity(t, home)
	cmd := exec.Command("sh", "-c", FreshScript(false))
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, name := range forgotten {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the claim: %v", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Errorf("the claim removed %s: %v", name, err)
		}
	}
}

func TestSSHKeyPairIsGeneratedOnceAndRemovedWhole(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	pair, err := SSHKeyPair(context.Background(), dir, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	public, err := pair.PublicKey()
	if err != nil || !strings.HasPrefix(string(public), "ssh-ed25519 ") || !strings.Contains(string(public), "cc-remote ws-1") {
		t.Errorf("public key = %q, %v", public, err)
	}
	info, err := os.Stat(pair.Private)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, %v", info.Mode(), err)
	}
	again, err := SSHKeyPair(context.Background(), dir, "ws-1")
	if err != nil || again != pair {
		t.Errorf("a second call regenerated the key: %+v, %v", again, err)
	}
	if public2, _ := again.PublicKey(); string(public2) != string(public) {
		t.Error("the key changed between calls")
	}
	if err := pair.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pair.Public); !errors.Is(err, os.ErrNotExist) {
		t.Error("the public key survived Remove")
	}
	if err := pair.Remove(); err != nil {
		t.Errorf("removing an absent pair: %v", err)
	}
}
