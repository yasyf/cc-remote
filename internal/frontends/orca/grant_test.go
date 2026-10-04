package orca_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const grantProject = "/workspaces/monorepo"

func grantHooks(n int) []orca.GrantedHook {
	hooks := make([]orca.GrantedHook, n)
	for i := range hooks {
		hooks[i] = orca.GrantedHook{Key: fmt.Sprintf("%s/.codex/hooks.json:event:%02d:0", probeHome, i), Hash: fmt.Sprintf("sha256:%064x", i+1)}
	}
	return hooks
}

func grantDoc(t *testing.T, mode orca.GrantMode, edit func(map[string]any)) string {
	t.Helper()
	events := map[string]any{"SessionStart": 1, "UserPromptSubmit": 1, "PreToolUse": 1, "PostToolUse": 1, "Stop": 1}
	if mode == orca.GrantFinal {
		events = map[string]any{"SessionStart": 2, "UserPromptSubmit": 2, "PreToolUse": 2, "PermissionRequest": 1, "PostToolUse": 2, "SubagentStart": 1, "SubagentStop": 1, "Stop": 2}
	}
	count := 0
	for _, n := range events {
		count += n.(int)
	}
	doc := map[string]any{
		"schema": 1, "mode": string(mode), "outcome": "exact", "reason": "", "event": "", "record": -1, "step": "",
		"home": probeHome, "root": probeRoot, "project": grantProject, "hooksSha256": probeDigest,
		"definitions": count, "events": events, "projectWrite": false, "hookWrites": 0, "hooks": grantHooks(count),
		"seconds": map[string]any{"initialize": 0.6, "total": 0.9},
	}
	if edit != nil {
		edit(doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func refusedDoc(mode orca.GrantMode, reason, event string, record int, step string) string {
	return fmt.Sprintf(`{"schema":1,"mode":%q,"outcome":"refused","reason":%q,"event":%q,"record":%d,"step":%q}`, mode, reason, event, record, step)
}

func TestParseHookGrantAcceptsOnlyTheExactDocument(t *testing.T) {
	pin := captainPins[0]
	tests := []struct {
		name string
		mode orca.GrantMode
		out  string
		want string
	}{
		{"fill that wrote", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["projectWrite"], d["hookWrites"] = true, 5 }), ""},
		{"claim without writes", orca.GrantClaim, grantDoc(t, orca.GrantClaim, nil), ""},
		{"final thirteen", orca.GrantFinal, grantDoc(t, orca.GrantFinal, nil), ""},
		{"claim that wrote trust", orca.GrantClaim, grantDoc(t, orca.GrantClaim, func(d map[string]any) { d["projectWrite"] = true }), "wrote more than its mode allows"},
		{"final that granted", orca.GrantFinal, grantDoc(t, orca.GrantFinal, func(d map[string]any) { d["hookWrites"] = 1 }), "wrote more than its mode allows"},
		{"fill beyond five", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["hookWrites"] = 6 }), "wrote more than its mode allows"},
		{"another mode", orca.GrantClaim, grantDoc(t, orca.GrantFill, nil), "another schema or mode"},
		{"another schema", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["schema"] = 2 }), "another schema or mode"},
		{"unknown field", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["config"] = leaked }), "no single result document"},
		{"two documents", orca.GrantFill, grantDoc(t, orca.GrantFill, nil) + grantDoc(t, orca.GrantFill, nil), "no single result document"},
		{"thirteen for a fresh fill", orca.GrantFill, strings.Replace(grantDoc(t, orca.GrantFinal, nil), `"mode":"final"`, `"mode":"fill"`, 1), "another set of definitions"},
		{"a hook short", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["hooks"] = grantHooks(4) }), "another set of definitions"},
		{"duplicate key", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) {
			hooks := grantHooks(5)
			hooks[1].Key = hooks[0].Key
			d["hooks"] = hooks
		}), "unique key and native hash"},
		{"computed hash shape", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) {
			hooks := grantHooks(5)
			hooks[2].Hash = probeDigest
			d["hooks"] = hooks
		}), "unique key and native hash"},
		{"another project", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["project"] = "/workspaces" }), "another project"},
		{"another plugin root", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["root"] = probeHome + "/elsewhere" }), "another Captain Hook root"},
		{"relative home", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["home"] = "home/agent" }), "no absolute runtime home"},
		{"no digest", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["hooksSha256"] = "" }), "no hooks digest"},
		{"unknown timing", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["seconds"] = map[string]any{"thread": 1} }), "unrecognized timing"},
		{"unknown outcome", orca.GrantFill, grantDoc(t, orca.GrantFill, func(d map[string]any) { d["outcome"] = "granted" }), "unrecognized outcome"},
		{"unknown refusal", orca.GrantFill, refusedDoc(orca.GrantFill, leaked, "", -1, ""), "unrecognized reason"},
		{"refusal at an unknown event", orca.GrantFill, refusedDoc(orca.GrantFill, "modified", "Notification", 0, ""), "unrecognized reason"},
		{"refusal at an unknown step", orca.GrantFill, refusedDoc(orca.GrantFill, "rpc", "", -1, "thread/start"), "unrecognized reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orca.ParseHookGrant([]byte(tt.out), pin, tt.mode, grantProject)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("ParseHookGrant = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("ParseHookGrant = %v, want %q", err, tt.want)
			case err != nil && strings.Contains(err.Error(), leaked):
				t.Errorf("the diagnostic echoes the document: %v", err)
			}
		})
	}
}

func TestParseHookGrantTypesRefusals(t *testing.T) {
	_, err := orca.ParseHookGrant([]byte(refusedDoc(orca.GrantClaim, "untrusted-hook", "Stop", 1, "")), captainPins[0], orca.GrantClaim, grantProject)
	var refusal orca.GrantRefusal
	if !errors.As(err, &refusal) || refusal != (orca.GrantRefusal{Mode: orca.GrantClaim, Reason: "untrusted-hook", Event: "Stop", Record: 1}) {
		t.Fatalf("ParseHookGrant = %v", err)
	}
	if got := refusal.Error(); got != "the claim grant of the worker's Codex trust refused: untrusted-hook at Stop record 1" {
		t.Errorf("Error() = %q", got)
	}
	_, err = orca.ParseHookGrant([]byte(refusedDoc(orca.GrantFill, "timeout", "", -1, "hooks/list")), captainPins[0], orca.GrantFill, grantProject)
	if !errors.As(err, &refusal) || err.Error() != "the fill grant of the worker's Codex trust refused: timeout during hooks/list" {
		t.Errorf("ParseHookGrant = %v", err)
	}
}

func TestHookGrantRunsOneHelperWithoutCredentials(t *testing.T) {
	var argv []string
	grant := orca.HookGrant{Captain: captainPins, Codex: "0.159.2", Project: grantProject, Exec: func(_ context.Context, got []string) ([]byte, error) {
		argv = got
		return []byte(grantDoc(t, orca.GrantFill, func(d map[string]any) { d["projectWrite"], d["hookWrites"] = true, 5 })), nil
	}}
	pregrant, err := grant.Fill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pregrant.Project != grantProject || !slices.Equal(pregrant.Hooks, grantHooks(5)) {
		t.Errorf("pregrant = %+v", pregrant)
	}
	want := append([]string{"python3", "-c", orca.HookGrantScript, "fill", "captain-hook@captain-hook", "12.79.15", "0.159.2", grantProject, "30"}, orca.CredentialEnv()...)
	if !slices.Equal(argv, want) {
		t.Errorf("argv tail = %q", argv[3:])
	}
	if slices.Contains(argv, "CODEX_HOME") {
		t.Error("the configuration target is stripped instead of bound")
	}
	if _, err := (orca.HookGrant{Captain: nil, Exec: grant.Exec}).Run(context.Background(), orca.GrantFill); err == nil {
		t.Error("a grant ran without exactly one Captain Hook pin")
	}
}

func TestHookGrantBindsClaimAndFinalToThePregrant(t *testing.T) {
	prior := orca.Pregrant{Project: grantProject, Hooks: grantHooks(5)}
	answer := func(out string) orca.HookGrant {
		return orca.HookGrant{Captain: captainPins, Codex: "0.159.2", Project: grantProject, Exec: func(context.Context, []string) ([]byte, error) { return []byte(out), nil }}
	}
	if err := answer(grantDoc(t, orca.GrantClaim, nil)).Claim(context.Background(), prior); err != nil {
		t.Errorf("an unchanged pregrant was refused: %v", err)
	}
	drifted := grantDoc(t, orca.GrantClaim, func(d map[string]any) {
		hooks := grantHooks(5)
		hooks[3].Hash = fmt.Sprintf("sha256:%064x", 99)
		d["hooks"] = hooks
	})
	if err := answer(drifted).Claim(context.Background(), prior); err == nil {
		t.Error("a claim accepted a hash its pregrant never trusted")
	}
	if err := answer(grantDoc(t, orca.GrantClaim, nil)).Claim(context.Background(), orca.Pregrant{Project: "/workspaces", Hooks: prior.Hooks}); err == nil {
		t.Error("a claim accepted another project's pregrant")
	}
	if err := answer(grantDoc(t, orca.GrantFinal, nil)).Final(context.Background(), prior); err != nil {
		t.Errorf("the final thirteen lost the five: %v", err)
	}
	lost := grantDoc(t, orca.GrantFinal, func(d map[string]any) {
		hooks := grantHooks(13)
		hooks[0].Hash = fmt.Sprintf("sha256:%064x", 99)
		d["hooks"] = hooks
	})
	if err := answer(lost).Final(context.Background(), prior); err == nil {
		t.Error("the final verification accepted a moved Captain Hook handler without its granted hash")
	}
}

func TestBootstrapOfAPregrantedCodexWorker(t *testing.T) {
	prior := orca.Pregrant{Project: grantProject, Hooks: grantHooks(5)}
	tests := []struct {
		name    string
		screens [][]string
		waits   []string
		final   string
		want    error
		execs   int
		idles   int
	}{
		{"verified before the final idle", [][]string{codexReady}, []string{idleWait}, grantDoc(t, orca.GrantFinal, nil), nil, 1, 1},
		{"hook review prompt", [][]string{promptScreen(5, "1. Review hooks")}, nil, "", orca.ErrPregrantPrompt, 0, 0},
		{"folder trust gate", [][]string{folderShown}, nil, "", orca.ErrPregrantPrompt, 0, 0},
		{"late review after the verification", [][]string{codexReady, promptScreen(8, "1. Review hooks")}, []string{blockedWait}, grantDoc(t, orca.GrantFinal, nil), orca.ErrPregrantPrompt, 1, 1},
		{"final refusal", [][]string{codexReady}, nil, refusedDoc(orca.GrantFinal, "untrusted-hook", "Stop", 1, ""), orca.GrantRefusal{Mode: orca.GrantFinal, Reason: "untrusted-hook", Event: "Stop", Record: 1}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t)
			for _, screen := range tt.screens {
				fake.on(readScreen, frameOf(t, "screen", false, false, screen))
			}
			for _, wait := range tt.waits {
				fake.on(waitIdle, ok(wait))
			}
			execs := 0
			grant := orca.HookGrant{Captain: captainPins, Codex: "0.159.2", Project: grantProject, Exec: func(_ context.Context, argv []string) ([]byte, error) {
				execs++
				if argv[3] != string(orca.GrantFinal) || fake.called(waitIdle) != 0 {
					t.Errorf("the %s verification ran after %d idle waits", argv[3], fake.called(waitIdle))
				}
				return []byte(tt.final), nil
			}}
			steps, err := orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.GrantedCodexStartup(grant, prior), true, refusalPoll)
			switch {
			case tt.want == nil && (err != nil || !slices.Equal(steps, []string{"hooks.granted"})):
				t.Fatalf("Bootstrap = %v, %v", steps, err)
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Fatalf("Bootstrap = %v, want %v", err, tt.want)
			}
			if execs != tt.execs || fake.called(waitIdle) != tt.idles || fake.called(sendEnter) != 0 {
				t.Errorf("execs = %d, idle waits = %d, enters = %d", execs, fake.called(waitIdle), fake.called(sendEnter))
			}
		})
	}
}
