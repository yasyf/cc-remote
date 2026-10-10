package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/version"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const guestLaunchRuntime = "tools/orca/squashfs-root/resources/bin/orca-ide"

var guestLaunchAgent = orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}

type guestLaunch struct {
	err        error
	task       *orcaTask
	orcaCalls  []string
	commands   []string
	execs      []string
	ssh        []string
	descriptor []byte
	logs       string
	urlCalls   string
}

func launchThroughGuest(t *testing.T, guest *helperGuest, claimed bool, result string) guestLaunch {
	t.Helper()
	original := version.Version
	version.Version = "0.20.0"
	t.Cleanup(func() { version.Version = original })
	fakeRemote(t)
	descriptor := filepath.Join(t.TempDir(), "descriptor.json")
	t.Setenv("GUEST_DESCRIPTOR", descriptor)
	t.Setenv("GUEST_RESULT", result)
	root, _ := gitCheckout(t)
	control, err := state.NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
	ready := `{"type":"orca_server_ready","schemaVersion":1,"runtimeId":"rt-1","boundEndpoint":"ws://0.0.0.0:7001","advertisedEndpoint":"ws://127.0.0.1:7001","pairing":{"available":true,"url":"orca://pair?code=private"}}`
	var run guestLaunch
	provider := &providertest.Fake{Handle: func(_ string, cmd []string, stdin []byte) providers.Result {
		if result, ok := guest.match(cmd, stdin); ok {
			return result
		}
		run.execs = append(run.execs, "runtime")
		return providers.Result{Stdout: []byte("starting\n" + ready + "\n")}
	}}
	if _, err := provider.Create(t.Context(), providers.Spec{Name: "task-a"}); err != nil {
		t.Fatal(err)
	}
	envelope := func(runtimeID, result string) string {
		return `{"ok":true,"result":` + result + `,"_meta":{"runtimeId":"` + runtimeID + `"}}`
	}
	worktree := "repo-1::" + root
	scope := " --environment task-a --json"
	run.commands = []string{
		"environment add --name task-a --pairing-code orca://pair?code=private --json",
		"status" + scope,
		"repo add --path " + root + scope,
		"terminal create --worktree id:" + worktree + " --title task-a --command " + guestLaunchAgent.Command(fakeKeyDir, orca.ShellPolicy{}) + scope,
	}
	fake := &scriptedOrca{t: t, replies: map[string]string{
		run.commands[0]: envelope("local", `{"environment":{"id":"env-1","name":"task-a"}}`),
		run.commands[1]: envelope("rt-1", `{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}}`),
		run.commands[2]: envelope("rt-1", `{"repo":{"id":"repo-1","path":"`+root+`"}}`),
		run.commands[3]: envelope("rt-1", `{"terminal":{"handle":"term-1","worktreeId":"`+worktree+`"}}`),
	}}
	dir := t.TempDir()
	record := filepath.Join(dir, "url-args")
	if err := os.WriteFile(filepath.Join(dir, "helper-url"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\nprintf '%s\\n' '"+helperURL+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cfg := &config.Config{Path: filepath.Join(dir, "config.yaml"), Orca: config.Orca{BootstrapHelper: testHelper()}}
	session := &workspace.Session{Config: cfg, Provider: provider, Kind: "sprites", Now: time.Now, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Stderr: io.Discard}
	driver := orcaDriver{client: orca.NewClient(fake), state: state.Dir(t.TempDir()), log: session.Log}
	run.task = &orcaTask{
		SchemaVersion: orcaTaskSchema, Workspace: "task-a", Provider: "fake", Profile: "lean", Machine: "task-a", ProjectRoot: root,
		Service: orca.RuntimeService, Port: 7001, Environment: "task-a", Agent: guestLaunchAgent, claimed: claimed,
		Forward: &orcaTunnel{Host: "task-a", Config: "/state/ssh/task-a.ssh", Control: control, Log: filepath.Join(t.TempDir(), "forward.log")},
	}
	run.err = driver.launch(t.Context(), session, run.task, orca.Runtime{Entry: guestLaunchRuntime}, secrets{key: []byte("sk-test-key"), judge: []byte("sk-judge-key")}, "task-a")
	run.orcaCalls, run.ssh, run.logs = fake.calls, sshCalls(t), logs.String()
	run.descriptor, _ = os.ReadFile(descriptor)
	raw, _ := os.ReadFile(record)
	run.urlCalls = string(raw)
	return run
}

func TestLaunchHandsTheBootstrapToTheAdmittedGuestHelper(t *testing.T) {
	tests := []struct {
		name   string
		result string
		steps  []string
		is     error
		want   string
	}{
		{"the guest answers every gate", `{"schema":1,"steps":["theme","api-key","bypass"]}`, []string{"theme", "api-key", "bypass"}, nil, ""},
		{"an untrusted checkout stays a typed refusal", `{"schema":1,"steps":["theme"],"kind":"untrusted","error":"` + orca.ErrUntrusted.Error() + `"}`, []string{"theme"}, orca.ErrUntrusted, "the guest bootstrap helper: "},
		{"a guest failure has no Mac fallback", `{"schema":1,"steps":[],"kind":"failed","error":"terminal term-1 on local was not tui-idle"}`, []string{}, nil, "was not tui-idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guest := &helperGuest{state: helperReady}
			run := launchThroughGuest(t, guest, false, tt.result)
			if tt.want == "" && run.err != nil || tt.want != "" && (run.err == nil || !strings.Contains(run.err.Error(), tt.want)) {
				t.Fatalf("launch = %v, want %q", run.err, tt.want)
			}
			if tt.is != nil && !errors.Is(run.err, tt.is) {
				t.Errorf("launch = %v, not %v", run.err, tt.is)
			}
			if !slices.Equal(run.task.Bootstrap, tt.steps) {
				t.Errorf("bootstrap steps = %q, want %q", run.task.Bootstrap, tt.steps)
			}
			if !slices.Equal(guest.calls, []string{"probe"}) || !slices.Equal(run.execs, []string{"runtime"}) || run.urlCalls != "" {
				t.Errorf("helper calls %q, provider execs %q, url_command %q; want one check before the runtime", guest.calls, run.execs, run.urlCalls)
			}
			if !slices.Equal(run.orcaCalls, run.commands) {
				t.Errorf("orca calls =\n%q\nwant\n%q, with no Mac screen read, key, or idle wait", run.orcaCalls, run.commands)
			}
			if !slices.Equal(run.ssh, []string{"check", "key-dir", "key-write", "guest-bootstrap"}) {
				t.Errorf("ssh calls = %q, want the helper once after the one key write", run.ssh)
			}
			var guestDescriptor orca.Guest
			decoder := json.NewDecoder(bytes.NewReader(run.descriptor))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&guestDescriptor); err != nil {
				t.Fatal(err)
			}
			want := orca.NewGuest(config.HelperPlatform, "rt-1", "term-1", guestLaunchRuntime, guestLaunchAgent, orca.ClaudeStartup, false, orca.Poll{Interval: time.Second, Timeout: 3 * time.Minute})
			if fmt.Sprint(guestDescriptor) != fmt.Sprint(want) || guestDescriptor.Version != "0.20.0" || guestDescriptor.CLI != "tools/orca/squashfs-root/resources/bin/orca-ide" {
				t.Errorf("descriptor = %+v, want %+v", guestDescriptor, want)
			}
			for _, private := range []string{"sk-test-key", "sk-judge-key", "orca://pair"} {
				if strings.Contains(string(run.descriptor), private) || strings.Contains(run.logs, private) {
					t.Errorf("the descriptor or logs carry %q", private)
				}
			}
			if strings.Contains(string(run.descriptor), guestLaunchAgent.Model) || strings.Contains(string(run.descriptor), guestLaunchAgent.Effort) {
				t.Errorf("the descriptor carries the model or effort: %s", run.descriptor)
			}
			for _, timing := range []string{`"operation":"helper.prepare"`, `"operation":"bootstrap.guest"`} {
				if !strings.Contains(run.logs, timing) {
					t.Errorf("logs have no %s", timing)
				}
			}
		})
	}
}

func TestLaunchKeepsAClaimedSpareVerifyOnly(t *testing.T) {
	staged := []string{"extract", "version " + stagedHelper + "/cc-remote", "promote " + stagedHelper, "probe"}
	tests := []struct {
		name     string
		claimed  bool
		state    string
		calls    []string
		urlCalls string
		execs    []string
		ssh      []string
		want     string
	}{
		{"a claimed spare whose helper vanished", true, helperAbsent, []string{"probe"}, "", nil, nil, "a claimed spare is never prepared"},
		{"a claimed spare with only the archive", true, helperArchive, []string{"probe"}, "", nil, nil, "a claimed spare is never prepared"},
		{"a claimed spare with its admitted helper", true, helperReady, []string{"probe"}, "", []string{"runtime"}, []string{"check", "key-dir", "key-write", "guest-bootstrap"}, ""},
		{"a cold workspace prepares its absent helper", false, helperAbsent, slices.Concat([]string{"probe", "fetch"}, staged), "bootstrap-helper " + helperSHA + " 6288446\n", []string{"runtime"}, []string{"check", "key-dir", "key-write", "guest-bootstrap"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guest := &helperGuest{state: tt.state, version: "0.20.0", extract: true}
			run := launchThroughGuest(t, guest, tt.claimed, `{"schema":1,"steps":["theme"]}`)
			if tt.want == "" && run.err != nil || tt.want != "" && (run.err == nil || !strings.Contains(run.err.Error(), tt.want)) {
				t.Fatalf("launch = %v, want %q", run.err, tt.want)
			}
			if !slices.Equal(guest.calls, tt.calls) || run.urlCalls != tt.urlCalls {
				t.Errorf("helper calls %q with url_command %q; want %q and %q", guest.calls, run.urlCalls, tt.calls, tt.urlCalls)
			}
			if !slices.Equal(run.execs, tt.execs) || !slices.Equal(run.ssh, tt.ssh) {
				t.Errorf("provider execs %q, ssh calls %q; want %q and %q", run.execs, run.ssh, tt.execs, tt.ssh)
			}
			if tt.want != "" && (len(run.orcaCalls) != 0 || run.task.Terminal != "" || len(run.descriptor) != 0) {
				t.Errorf("a refused claim reached orca %q, terminal %q, descriptor %q", run.orcaCalls, run.task.Terminal, run.descriptor)
			}
		})
	}
}
