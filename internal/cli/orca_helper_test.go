package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/version"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	helperSHA    = "1ad72b8a5d119065a865f0e21bb4af77bb50e62b340a3cd1bb3a76470a207dae"
	helperBinary = "c7d716f9d273b6c58b5424fadb79877c772ba4c6290aadb4d36764aeef511d57"
	helperURL    = "https://helpers.example/cc-remote.tar.gz?X-Amz-Signature=helper-sentinel"
	stagedHelper = images.HelperStore + "/" + helperSHA + ".Ab12Cd34.partial"
)

func testHelper() *config.BootstrapHelper {
	return &config.BootstrapHelper{Source: config.Source{URLCommand: []string{"./helper-url", "bootstrap-helper"}, SHA256: helperSHA, Size: 6288446}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: helperBinary}
}

func TestSelectHelperPinsTheRunningSpritesRelease(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })
	tests := []struct {
		name    string
		kind    string
		helper  *config.BootstrapHelper
		running string
		want    bool
		err     string
	}{
		{"no helper keeps the Mac bootstrap", "sprites", nil, "0.20.0", false, ""},
		{"namespace keeps the Mac bootstrap", "namespace", testHelper(), "0.20.0", false, ""},
		{"the running sprites release", "sprites", testHelper(), "0.20.0", true, ""},
		{"another running release", "sprites", testHelper(), "0.21.0", false, "orca.bootstrap_helper pins cc-remote 0.20.0, and this is cc-remote 0.21.0"},
		{"an unstamped build", "sprites", testHelper(), "dev", false, "and this is cc-remote dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version.Version = tt.running
			session := &workspace.Session{Config: &config.Config{Orca: config.Orca{BootstrapHelper: tt.helper}}, Kind: tt.kind}
			helper, err := selectHelper(session)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("selectHelper = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || (helper != nil) != tt.want {
				t.Fatalf("selectHelper = %+v, %v; want selected %t", helper, err, tt.want)
			}
		})
	}
}

type helperGuest struct {
	state   string
	version string
	extract bool
	calls   []string
	fetches []string
}

func (g *helperGuest) handle(_ string, cmd []string, stdin []byte) providers.Result {
	if result, ok := g.match(cmd, stdin); ok {
		return result
	}
	g.calls = append(g.calls, "unexpected "+strings.Join(cmd, " "))
	return providers.Result{ExitCode: 9}
}

func (g *helperGuest) match(cmd []string, stdin []byte) (providers.Result, bool) {
	at := slices.Index(cmd, "-c")
	if at < 0 || at+2 >= len(cmd) {
		return providers.Result{}, false
	}
	script, args := cmd[at+1], cmd[at+2:]
	switch {
	case cmd[0] == "sh" && script == helperProbeScript:
		g.calls = append(g.calls, "probe")
		if !slices.Equal(args, []string{"probe-helper", helperSHA, helperBinary, admissionOf(testHelper())}) {
			return providers.Result{ExitCode: 2, Stderr: []byte("probe arguments")}, true
		}
		return providers.Result{Stdout: []byte(g.state + "\n")}, true
	case cmd[0] == "sudo" && args[0] == "fetch-"+images.HelperArtifact.Label:
		g.calls = append(g.calls, "fetch")
		g.fetches = append(g.fetches, strings.Join(args, " ")+" | "+string(stdin))
		g.state = helperArchive
		return providers.Result{}, true
	case cmd[0] == "sudo" && script == helperExtractScript:
		g.calls = append(g.calls, "extract")
		if !g.extract {
			return providers.Result{ExitCode: 1, Stderr: []byte("cc-remote: the archive holds 2 cc-remote members, want exactly one")}, true
		}
		return providers.Result{Stdout: []byte(stagedHelper + "\n")}, true
	case cmd[0] == "sh" && script == `exec "$1" version 2>&1`:
		g.calls = append(g.calls, "version "+args[1])
		return providers.Result{Stdout: []byte(g.version + "\n")}, true
	case cmd[0] == "sudo" && script == helperPromoteScript:
		g.calls = append(g.calls, "promote "+args[2])
		g.state = helperReady
		return providers.Result{}, true
	}
	return providers.Result{}, false
}

func helperSession(t *testing.T, guest *helperGuest, urlScript string) (*workspace.Session, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "url-args")
	if err := os.WriteFile(filepath.Join(dir, "helper-url"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"+urlScript+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	provider := &providertest.Fake{Handle: guest.handle}
	if _, err := provider.Create(context.Background(), providers.Spec{Name: "pool-a"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Path: filepath.Join(dir, "config.yaml"), Orca: config.Orca{BootstrapHelper: testHelper()}}
	return &workspace.Session{Config: cfg, Provider: provider, Kind: "sprites", Now: time.Now, Log: slog.New(slog.DiscardHandler), Stderr: io.Discard}, record
}

func TestEnsureHelperFetchesOnlyAnAbsentHelperAndAdmitsItOnce(t *testing.T) {
	printURL := "printf '%s\\n' '" + helperURL + "'"
	tests := []struct {
		name      string
		guest     helperGuest
		urlScript string
		calls     []string
		urlCalls  int
		want      string
	}{
		{"a cache hit downloads nothing", helperGuest{state: helperReady}, printURL, []string{"probe"}, 0, ""},
		{"an admitted archive is extracted without a download", helperGuest{state: helperArchive, version: "0.20.0", extract: true}, printURL, []string{"probe", "extract", "version " + stagedHelper + "/cc-remote", "promote " + stagedHelper, "probe"}, 0, ""},
		{"an absent helper is fetched once", helperGuest{state: helperAbsent, version: "0.20.0", extract: true}, printURL, []string{"probe", "fetch", "extract", "version " + stagedHelper + "/cc-remote", "promote " + stagedHelper, "probe"}, 1, ""},
		{"a staged helper of another release is never admitted", helperGuest{state: helperArchive, version: "0.19.0", extract: true}, printURL, []string{"probe", "extract", "version " + stagedHelper + "/cc-remote"}, 0, `is cc-remote "0.19.0", want 0.20.0`},
		{"a refused extraction never runs the binary", helperGuest{state: helperArchive}, printURL, []string{"probe", "extract"}, 0, "holds 2 cc-remote members"},
		{"a failed url_command fetches nothing", helperGuest{state: helperAbsent}, "exit 3", []string{"probe"}, 1, "bootstrap helper archive url_command ./helper-url: exit status 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guest := tt.guest
			session, record := helperSession(t, &guest, tt.urlScript)
			err := ensureHelper(context.Background(), session, "pool-a", testHelper())
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("ensureHelper = %v, want %q", err, tt.want)
			}
			if !slices.Equal(guest.calls, tt.calls) {
				t.Errorf("guest calls = %q, want %q", guest.calls, tt.calls)
			}
			raw, _ := os.ReadFile(record)
			if got := strings.Count(string(raw), "\n"); got != tt.urlCalls || tt.urlCalls == 1 && string(raw) != "bootstrap-helper "+helperSHA+" 6288446\n" {
				t.Errorf("url_command ran %d times with %q, want %d", got, raw, tt.urlCalls)
			}
			for _, fetch := range guest.fetches {
				if want := "fetch-bootstrap-helper " + helperSHA + " 6288446 | url = \"" + helperURL + "\"\n"; fetch != want {
					t.Errorf("fetch = %q, want %q", fetch, want)
				}
			}
			for _, call := range guest.calls {
				if strings.Contains(call, "helper-sentinel") {
					t.Errorf("a guest argv carries the signed URL: %q", call)
				}
			}
		})
	}
}

func TestAClaimVerifiesTheHelperWithoutPreparingIt(t *testing.T) {
	original := version.Version
	version.Version = "0.20.0"
	t.Cleanup(func() { version.Version = original })
	for _, state := range []string{helperAbsent, helperArchive} {
		t.Run(state, func(t *testing.T) {
			guest := helperGuest{state: state}
			session, record := helperSession(t, &guest, "printf '%s\\n' '"+helperURL+"'")
			if err := claimHelper(context.Background(), session, "pool-a"); err == nil || !strings.Contains(err.Error(), "a claimed spare is never prepared") {
				t.Fatalf("claimHelper = %v", err)
			}
			if !slices.Equal(guest.calls, []string{"probe"}) {
				t.Errorf("guest calls = %q, want only the check", guest.calls)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Errorf("a claim ran the url_command: %v", err)
			}
		})
	}
	guest := helperGuest{state: helperReady}
	session, _ := helperSession(t, &guest, "exit 3")
	if err := claimHelper(context.Background(), session, "pool-a"); err != nil || !slices.Equal(guest.calls, []string{"probe"}) {
		t.Errorf("claimHelper of a ready helper = %v with calls %q", err, guest.calls)
	}
}
