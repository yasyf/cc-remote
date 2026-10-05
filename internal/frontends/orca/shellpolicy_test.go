package orca_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func TestShellPolicyReadRunsThePinnedReader(t *testing.T) {
	var got []string
	read := orca.ShellPolicyRead{Codex: "0.159.2", Project: "/home/u/repo", Exec: func(_ context.Context, argv []string) ([]byte, error) {
		got = argv
		return []byte(`{"schema":1,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":true,"exclude":["AWS_*"]}`), nil
	}}
	policy, err := read.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"python3", "-c", orca.ShellPolicyScript, "0.159.2", "/home/u/repo", "30"}, orca.CredentialEnv()...)
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got[3:], want[3:])
	}
	if !policy.Legacy || !slices.Equal(policy.Exclude, []string{"AWS_*"}) {
		t.Errorf("policy = %+v", policy)
	}
}

func TestShellPolicyReadNeedsAPinnedCodex(t *testing.T) {
	read := orca.ShellPolicyRead{Project: "/home/u/repo", Exec: func(context.Context, []string) ([]byte, error) {
		t.Fatal("the reader ran without a pinned codex")
		return nil, nil
	}}
	if _, err := read.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "pins no codex version") {
		t.Errorf("Run = %v", err)
	}
}

func TestParseShellPolicy(t *testing.T) {
	refused := func(reason, layer, record, step string) string {
		return `{"schema":1,"outcome":"refused","reason":"` + reason + `","layer":"` + layer + `","record":` + record + `,"step":"` + step + `"}`
	}
	tests := []struct {
		name    string
		out     string
		want    orca.ShellPolicy
		refusal *orca.ShellPolicyRefusal
		err     string
	}{
		{"keyed", `{"schema":1,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":false,"exclude":[]}`, orca.ShellPolicy{}, nil, ""},
		{"legacy", `{"schema":1,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":true,"exclude":["AWS_*","[!A]?_TOKEN","A\"B\\C"]}`, orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*", "[!A]?_TOKEN", `A"B\C`}}, nil, ""},
		{"set in the project layer", refused("set", "project", "1", ""), orca.ShellPolicy{}, &orca.ShellPolicyRefusal{Reason: "set", Layer: "project", Record: 1}, "set at the project layer, record 1"},
		{"managed layer", refused("managed", "legacyManagedConfigTomlFromFile", "0", ""), orca.ShellPolicy{}, &orca.ShellPolicyRefusal{Reason: "managed", Layer: "legacyManagedConfigTomlFromFile"}, "managed at the legacyManagedConfigTomlFromFile layer, record 0"},
		{"another codex", refused("codex", "", "-1", ""), orca.ShellPolicy{}, &orca.ShellPolicyRefusal{Reason: "codex", Record: -1}, "API keys out of tool commands: codex"},
		{"timeout", refused("timeout", "", "-1", "config/read"), orca.ShellPolicy{}, &orca.ShellPolicyRefusal{Reason: "timeout", Record: -1, Step: "config/read"}, "timeout during config/read"},
		{"unknown reason", refused("weakened", "", "-1", ""), orca.ShellPolicy{}, nil, "unrecognized reason"},
		{"unknown layer", refused("set", "thread", "0", ""), orca.ShellPolicy{}, nil, "unrecognized reason"},
		{"unknown step", refused("rpc", "", "-1", "hooks/list"), orca.ShellPolicy{}, nil, "unrecognized reason"},
		{"unknown outcome", `{"schema":1,"outcome":"partial","reason":"","layer":"","record":-1,"step":""}`, orca.ShellPolicy{}, nil, "unrecognized outcome"},
		{"another schema", `{"schema":2,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":false,"exclude":[]}`, orca.ShellPolicy{}, nil, "another schema"},
		{"extra field", `{"schema":1,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":false,"exclude":[],"set":{}}`, orca.ShellPolicy{}, nil, "no single result document"},
		{"two documents", `{"schema":1,"outcome":"exact","reason":"","layer":"","record":-1,"step":"","legacy":false,"exclude":[]}{}`, orca.ShellPolicy{}, nil, "no single result document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orca.ParseShellPolicy([]byte(tt.out))
			if tt.err == "" {
				if err != nil || got.Legacy != tt.want.Legacy || !slices.Equal(got.Exclude, tt.want.Exclude) {
					t.Fatalf("ParseShellPolicy = %+v, %v; want %+v", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("ParseShellPolicy = %+v, %v; want %q", got, err, tt.err)
			}
			var refusal orca.ShellPolicyRefusal
			if errors.As(err, &refusal) != (tt.refusal != nil) || tt.refusal != nil && refusal != *tt.refusal {
				t.Errorf("refusal = %+v, want %+v", refusal, tt.refusal)
			}
		})
	}
}
