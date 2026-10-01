package providers_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

func TestSSHOptions(t *testing.T) {
	tests := []struct {
		name   string
		target providers.Target
		want   []string
	}{
		{
			"pinned behind a proxy",
			providers.Target{
				Host: "h", Port: 22, User: "u", IdentityFile: "/k", ProxyCommand: "proxy h",
				HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyPinned, Alias: "h.alias", KnownHostsFile: "/known"},
			},
			[]string{"HostName=h", "Port=22", "User=u", `IdentityFile="/k"`, "IdentitiesOnly=yes", "ProxyCommand=proxy h", "HostKeyAlias=h.alias", `UserKnownHostsFile="/known"`, "StrictHostKeyChecking=yes"},
		},
		{
			"proxy trusted with no key of its own",
			providers.Target{Host: "h", Port: 22, User: "u", ProxyCommand: "proxy h", HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyProxyTrusted}},
			[]string{"HostName=h", "Port=22", "User=u", "ProxyCommand=proxy h", "UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=no"},
		},
		{
			"direct and pinned",
			providers.Target{
				Host: "h.tailnet", Port: 2222, User: "u",
				HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyPinned, Alias: "h.alias", KnownHostsFile: "/known"},
			},
			[]string{"HostName=h.tailnet", "Port=2222", "User=u", "HostKeyAlias=h.alias", `UserKnownHostsFile="/known"`, "StrictHostKeyChecking=yes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.target.SSHOptions(); !slices.Equal(got, tt.want) {
				t.Errorf("SSHOptions() =\n%q\nwant\n%q", got, tt.want)
			}
			parsed := providertest.OpenSSHConfig(t, tt.target)
			if parsed["hostname"] != tt.target.Host || parsed["port"] != strconv.Itoa(tt.target.Port) {
				t.Errorf("ssh -G read %s:%s", parsed["hostname"], parsed["port"])
			}
		})
	}
	defer func() {
		if recover() == nil {
			t.Error("SSHOptions with no host key mode did not panic")
		}
	}()
	providers.Target{Host: "h", Port: 22, User: "u"}.SSHOptions()
}

func TestSSHOptionsKeepSpacesAndPercentSigns(t *testing.T) {
	target := providers.Target{
		Host:         "h",
		Port:         22,
		User:         "u",
		IdentityFile: "/tmp/a b/100%/k",
		ProxyCommand: providers.ShellQuote("/tmp/100%/cc-remote", "proxy", "--", "/x y/sprite", "-W", ":22"),
		HostKeyPolicy: providers.HostKeyPolicy{
			Mode:           providers.HostKeyPinned,
			Alias:          "h.alias",
			KnownHostsFile: "/tmp/a b/known_hosts",
		},
	}
	parsed := providertest.OpenSSHConfig(t, target)
	want := map[string]string{
		"identityfile":          "/tmp/a b/100%%/k",
		"proxycommand":          "/tmp/100%%/cc-remote proxy -- '/x y/sprite' -W :22",
		"hostkeyalias":          "h.alias",
		"userknownhostsfile":    "/tmp/a b/known_hosts",
		"stricthostkeychecking": "true",
	}
	for key, value := range want {
		if parsed[key] != value {
			t.Errorf("ssh parsed %s = %q, want %q", key, parsed[key], value)
		}
	}
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
		if got := providers.ShellQuote(tt.words...); got != tt.want {
			t.Errorf("providers.ShellQuote(%q) = %s, want %s", tt.words, got, tt.want)
		}
		out, err := exec.Command("sh", "-c", `for w in `+providers.ShellQuote(tt.words...)+`; do printf '%s\0' "$w"; done`).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !slices.Equal(got, tt.words) {
			t.Errorf("sh split %s into %q, want %q", providers.ShellQuote(tt.words...), got, tt.words)
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
		if err := providers.CheckName(tt.name, 5); (err == nil) != tt.ok {
			t.Errorf("providers.CheckName(%q) = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

func TestRecords(t *testing.T) {
	records := providers.Records{Dir: filepath.Join(t.TempDir(), "machines")}
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
	result, err := providers.OSRunner{}.Run(t.Context(), providers.Command{Name: "sh", Args: []string{"-c", "cat; echo e >&2; exit 4"}, Stdin: strings.NewReader("in")})
	if err != nil || string(result.Stdout) != "in" || string(result.Stderr) != "e\n" || result.ExitCode != 4 {
		t.Errorf("Run = %+v, %v", result, err)
	}
	if _, err := (providers.OSRunner{}).Run(t.Context(), providers.Command{Name: "cc-remote-no-such-binary"}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Run of a missing binary = %v, want exec.ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (providers.OSRunner{}).Run(ctx, providers.Command{Name: "sleep", Args: []string{"5"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("Run with a canceled context = %v, want context.Canceled", err)
	}
}

func TestOutput(t *testing.T) {
	out, err := providers.Output(t.Context(), providers.OSRunner{}, providers.Command{Name: "printf", Args: []string{"ok"}})
	if err != nil || string(out) != "ok" {
		t.Errorf("Output = %q, %v", out, err)
	}
	_, err = providers.Output(t.Context(), providers.OSRunner{}, providers.Command{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 2"}})
	var failed *providers.CommandError
	if !errors.As(err, &failed) || err.Error() != "sh -c echo boom >&2; exit 2: exit 2: boom" {
		t.Errorf("Output = %v, want a providers.CommandError", err)
	}
}
