package providers

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSSHOptions(t *testing.T) {
	base := Target{Host: "h", Port: 22, User: "u", IdentityFile: "/k", ProxyCommand: "proxy h"}
	common := []string{"HostName=h", "Port=22", "User=u", "IdentityFile=/k", "IdentitiesOnly=yes", "ProxyCommand=proxy h"}
	tests := []struct {
		name   string
		policy HostKeyPolicy
		want   []string
	}{
		{"pinned", HostKeyPolicy{Mode: HostKeyPinned, Alias: "h.alias", KnownHostsFile: "/known"}, []string{"HostKeyAlias=h.alias", "UserKnownHostsFile=/known", "StrictHostKeyChecking=yes"}},
		{"proxy trusted", HostKeyPolicy{Mode: HostKeyProxyTrusted}, []string{"UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=no"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := base
			target.HostKeyPolicy = tt.policy
			if got, want := target.SSHOptions(), append(slices.Clone(common), tt.want...); !slices.Equal(got, want) {
				t.Errorf("SSHOptions() = %q, want %q", got, want)
			}
		})
	}
	defer func() {
		if recover() == nil {
			t.Error("SSHOptions with no host key mode did not panic")
		}
	}()
	base.SSHOptions()
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		words []string
		want  string
	}{
		{[]string{"printf", "%s", "a/b.c"}, "printf %s a/b.c"},
		{[]string{"a b", "it's", ""}, `'a b' 'it'\''s' ''`},
		{[]string{"$HOME", "*", "x;y"}, `'$HOME' '*' 'x;y'`},
	}
	for _, tt := range tests {
		if got := ShellQuote(tt.words...); got != tt.want {
			t.Errorf("ShellQuote(%q) = %s, want %s", tt.words, got, tt.want)
		}
		out, err := exec.Command("sh", "-c", `for w in `+ShellQuote(tt.words...)+`; do printf '%s\0' "$w"; done`).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !slices.Equal(got, tt.words) {
			t.Errorf("sh split %s into %q, want %q", ShellQuote(tt.words...), got, tt.words)
		}
	}
}

func TestCheckName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"alpha", true},
		{"a-1", true},
		{"a", true},
		{"abcde", true},
		{"abcdef", false},
		{"", false},
		{"Alpha", false},
		{"-a", false},
		{"a-", false},
		{"a_b", false},
		{"../a", false},
		{"a/b", false},
	}
	for _, tt := range tests {
		if err := CheckName(tt.name, 5); (err == nil) != tt.ok {
			t.Errorf("CheckName(%q) = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

func TestRecords(t *testing.T) {
	records := Records{Dir: filepath.Join(t.TempDir(), "machines")}
	labels, err := records.Labels("alpha")
	if err != nil || labels != nil {
		t.Fatalf("Labels of an unrecorded machine = %v, %v", labels, err)
	}
	want := map[string]string{"team": "a", "cc-remote/workspace": "w1"}
	if err := records.Save("alpha", want); err != nil {
		t.Fatal(err)
	}
	if got, err := records.Labels("alpha"); err != nil || !maps.Equal(got, want) {
		t.Errorf("Labels = %v, %v; want %v", got, err, want)
	}
	entries, err := os.ReadDir(records.Dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "alpha.json" {
		t.Errorf("records dir holds %v, %v; want only alpha.json", entries, err)
	}
	if err := records.Remove("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := records.Remove("alpha"); err != nil {
		t.Errorf("removing a missing record = %v", err)
	}
	if got, err := records.Labels("alpha"); err != nil || got != nil {
		t.Errorf("Labels after Remove = %v, %v", got, err)
	}
}

func TestOSRunner(t *testing.T) {
	result, err := OSRunner{}.Run(t.Context(), Command{Name: "sh", Args: []string{"-c", "cat; echo e >&2; exit 4"}, Stdin: strings.NewReader("in")})
	if err != nil || string(result.Stdout) != "in" || string(result.Stderr) != "e\n" || result.ExitCode != 4 {
		t.Errorf("Run = %+v, %v", result, err)
	}
	if _, err := (OSRunner{}).Run(t.Context(), Command{Name: "cc-remote-no-such-binary"}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Run of a missing binary = %v, want exec.ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (OSRunner{}).Run(ctx, Command{Name: "sleep", Args: []string{"5"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("Run with a canceled context = %v, want context.Canceled", err)
	}
}

func TestOutput(t *testing.T) {
	out, err := Output(t.Context(), OSRunner{}, Command{Name: "printf", Args: []string{"ok"}})
	if err != nil || string(out) != "ok" {
		t.Errorf("Output = %q, %v", out, err)
	}
	_, err = Output(t.Context(), OSRunner{}, Command{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 2"}})
	var failed *CommandError
	if !errors.As(err, &failed) || err.Error() != "sh -c echo boom >&2; exit 2: exit 2: boom" {
		t.Errorf("Output = %v, want a CommandError", err)
	}
}
