package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/cc-remote/internal/version"
)

const (
	GuestCLI    = "resources/bin/orca-ide"
	guestSchema = 1
	userDataDir = ".cc-remote/orca/user-data"
)

var guestRouting = []string{"ORCA_PAIRING_CODE", "ORCA_REMOTE_PAIRING", "ORCA_ENVIRONMENT"}

type GuestReview struct {
	Captain []Pin `json:"captain"`
}

type GuestGrant struct {
	Captain []Pin    `json:"captain"`
	Codex   string   `json:"codex"`
	Project string   `json:"project"`
	Prior   Pregrant `json:"prior"`
}

type Guest struct {
	Schema    int           `json:"schema"`
	Version   string        `json:"version"`
	Platform  string        `json:"platform"`
	RuntimeID string        `json:"runtimeId"`
	Terminal  string        `json:"terminal"`
	CLI       string        `json:"cli"`
	Agent     string        `json:"agent"`
	Trusted   bool          `json:"trusted"`
	Interval  time.Duration `json:"interval"`
	Timeout   time.Duration `json:"timeout"`
	Review    *GuestReview  `json:"review,omitempty"`
	Grant     *GuestGrant   `json:"grant,omitempty"`
}

type GuestResult struct {
	Schema int      `json:"schema"`
	Steps  []string `json:"steps"`
	Kind   string   `json:"kind,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type GuestError struct {
	Message string
	kind    error
}

var guestKinds = map[string]error{"untrusted": ErrUntrusted, "pregrant": ErrPregrantPrompt, "failed": nil}

func NewGuest(platform, runtimeID, terminal, cli string, agent Agent, startup Startup, trusted bool, p Poll) Guest {
	guest := Guest{Schema: guestSchema, Version: version.String(), Platform: platform, RuntimeID: runtimeID, Terminal: terminal, CLI: cli, Agent: agent.Kind, Trusted: trusted, Interval: p.Interval, Timeout: p.Timeout}
	switch {
	case startup.Granted != nil:
		grant := startup.Granted.Grant
		guest.Grant = &GuestGrant{Captain: grant.Captain, Codex: grant.Codex, Project: grant.Project, Prior: startup.Granted.Prior}
	case startup.Hooks != nil:
		guest.Review = &GuestReview{Captain: startup.Hooks.Captain}
	}
	return guest
}

func (g Guest) Run(ctx context.Context, home string, local func(context.Context, []string) ([]byte, error)) ([]string, error) {
	switch platform := runtime.GOOS + "/" + runtime.GOARCH; {
	case g.Schema != guestSchema:
		return nil, fmt.Errorf("the guest bootstrap descriptor is schema %d, not %d", g.Schema, guestSchema)
	case g.Version != version.String():
		return nil, fmt.Errorf("the guest helper is cc-remote %s, and the Mac runs cc-remote %s", version.String(), g.Version)
	case g.Platform != platform:
		return nil, fmt.Errorf("the guest helper runs on %s, and the descriptor names %s", platform, g.Platform)
	case g.Agent != AgentClaude && g.Agent != AgentCodex:
		return nil, fmt.Errorf("the guest bootstrap descriptor names agent %q", g.Agent)
	case g.Agent == AgentClaude && (g.Review != nil || g.Grant != nil):
		return nil, errors.New("the guest bootstrap descriptor gives claude a Codex hook check")
	case g.Agent == AgentCodex && (g.Review == nil) == (g.Grant == nil):
		return nil, errors.New("the guest bootstrap descriptor gives codex neither or both of its hook review and grant")
	case g.RuntimeID == "" || g.Terminal == "":
		return nil, errors.New("the guest bootstrap descriptor names no runtime or terminal")
	case g.Interval <= 0 || g.Timeout <= 0:
		return nil, fmt.Errorf("the guest bootstrap descriptor polls every %s for %s", g.Interval, g.Timeout)
	case !filepath.IsLocal(g.CLI) || filepath.Clean(g.CLI) != g.CLI:
		return nil, fmt.Errorf("the guest Orca CLI %q is not a clean path under the guest home", g.CLI)
	}
	userData := filepath.Join(home, userDataDir)
	if err := ownedDir(userData); err != nil {
		return nil, err
	}
	cli := filepath.Join(home, g.CLI)
	if info, err := os.Stat(cli); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("the guest Orca CLI %s is not an executable regular file", cli)
	}
	native := NewClient(ExecRunner{Command: cli, Env: guestEnv(os.Environ(), userData)}).Local(g.RuntimeID)
	if _, err := native.Status(ctx); err != nil {
		return nil, err
	}
	return native.Bootstrap(ctx, g.Terminal, g.startup(local), g.Trusted, Poll{Interval: g.Interval, Timeout: g.Timeout})
}

func (g Guest) startup(local func(context.Context, []string) ([]byte, error)) Startup {
	switch {
	case g.Grant != nil:
		return GrantedCodexStartup(HookGrant{Captain: g.Grant.Captain, Codex: g.Grant.Codex, Project: g.Grant.Project, Exec: local}, g.Grant.Prior)
	case g.Review != nil:
		return CodexStartup(HookReview{Captain: g.Review.Captain, Exec: local})
	}
	return StartupOf(Agent{Kind: g.Agent})
}

func ownedDir(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return fmt.Errorf("the guest Orca user data: %w", err)
	case !info.IsDir():
		return fmt.Errorf("the guest Orca user data %s is not a directory", path)
	case int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid():
		return fmt.Errorf("the guest Orca user data %s is owned by uid %d, not this user (uid %d)", path, info.Sys().(*syscall.Stat_t).Uid, os.Getuid())
	}
	return nil
}

func guestEnv(environ []string, userData string) []string {
	env := slices.DeleteFunc(slices.Clone(environ), func(pair string) bool {
		name, _, _ := strings.Cut(pair, "=")
		return name == "ORCA_USER_DATA_PATH" || slices.Contains(guestRouting, name)
	})
	return append(env, "ORCA_USER_DATA_PATH="+userData)
}

func LocalExec(ctx context.Context, argv []string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s on the guest: %w", filepath.Base(argv[0]), err)
	}
	return out, nil
}

func GuestOutcome(steps []string, err error) GuestResult {
	result := GuestResult{Schema: guestSchema, Steps: steps}
	if err == nil {
		return result
	}
	result.Kind, result.Error = "failed", err.Error()
	for kind, sentinel := range guestKinds {
		if sentinel != nil && errors.Is(err, sentinel) {
			result.Kind = kind
		}
	}
	return result
}

func ParseGuestResult(out []byte) ([]string, error) {
	var result GuestResult
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.More() || result.Schema != guestSchema {
		return nil, errors.New("the guest bootstrap helper printed no single result document")
	}
	kind, known := guestKinds[result.Kind]
	switch {
	case result.Kind == "" && result.Error == "":
		return result.Steps, nil
	case !known || result.Error == "":
		return result.Steps, fmt.Errorf("the guest bootstrap helper reported an unrecognized %q failure", result.Kind)
	}
	return result.Steps, GuestError{Message: result.Error, kind: kind}
}

func (e GuestError) Error() string { return "the guest bootstrap helper: " + e.Message }

func (e GuestError) Unwrap() error { return e.kind }
