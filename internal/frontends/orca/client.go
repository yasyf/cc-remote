package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const sshHostPrefix = "ssh:"

type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type ExecRunner struct {
	Command string
}

type Client struct {
	runner Runner
}

type Status struct {
	Runtime struct {
		State string `json:"state"`
	} `json:"runtime"`
	App struct {
		DesktopWindowStatus string `json:"desktopWindowStatus"`
	} `json:"app"`
}

type Repo struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	DisplayName string `json:"displayName"`
}

type Worktree struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	HostID      string `json:"hostId"`
	RepoID      string `json:"repoId"`
	Path        string `json:"path"`
	Branch      string `json:"branch"`
}

type Host struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

type Terminal struct {
	Handle          string   `json:"handle"`
	ExecutionHostID string   `json:"executionHostId"`
	Tail            []string `json:"tail"`
}

type DoctorCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type DoctorReport struct {
	RecipeID string        `json:"recipeId"`
	Checks   []DoctorCheck `json:"checks"`
}

func CLICommand(getenv func(string) string, goos string) string {
	switch {
	case getenv("ORCA_CLI_COMMAND") != "":
		return getenv("ORCA_CLI_COMMAND")
	case getenv("ORCA_DEV_REPO_ROOT") != "":
		return "orca-dev"
	case goos == "linux" && getenv("ORCA_TERMINAL_HANDLE") == "":
		return "orca-ide"
	default:
		return "orca"
	}
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", r.Command, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func NewClient(runner Runner) Client {
	return Client{runner: runner}
}

func (w Worktree) OnSSH() bool {
	return strings.HasPrefix(w.HostID, sshHostPrefix)
}

func (c Client) Status(ctx context.Context) (Status, error) {
	return call[Status](ctx, c, "status")
}

func (c Client) Repos(ctx context.Context) ([]Repo, error) {
	result, err := call[struct {
		Repos []Repo `json:"repos"`
	}](ctx, c, "repo", "list")
	return result.Repos, err
}

func (c Client) Worktrees(ctx context.Context) ([]Worktree, error) {
	result, err := call[struct {
		Worktrees []Worktree `json:"worktrees"`
	}](ctx, c, "worktree", "list", "--limit", "10000")
	return result.Worktrees, err
}

func (c Client) Hosts(ctx context.Context) ([]Host, error) {
	result, err := call[struct {
		Hosts []Host `json:"hosts"`
	}](ctx, c, "host", "list")
	return result.Hosts, err
}

func (c Client) OpenFile(ctx context.Context, worktreeID, file string) error {
	_, err := c.runner.Run(ctx, "file", "open", file, "--worktree", "id:"+worktreeID, "--json")
	return err
}

func (c Client) CreateTerminal(ctx context.Context, worktreeID, title, command string) (Terminal, error) {
	result, err := call[struct {
		Terminal Terminal `json:"terminal"`
	}](ctx, c, "terminal", "create", "--worktree", "id:"+worktreeID, "--title", title, "--command", command)
	return result.Terminal, err
}

func (c Client) ReadTerminal(ctx context.Context, handle string) (Terminal, error) {
	result, err := call[struct {
		Terminal Terminal `json:"terminal"`
	}](ctx, c, "terminal", "read", "--terminal", handle)
	return result.Terminal, err
}

func (c Client) Doctor(ctx context.Context, recipeID, repoPath string) (DoctorReport, error) {
	out, runErr := c.runner.Run(ctx, "vm", "recipe", "doctor", recipeID, "--repo-path", repoPath, "--json")
	var report DoctorReport
	if err := json.Unmarshal(out, &report); err != nil {
		return DoctorReport{}, errors.Join(runErr, fmt.Errorf("decode orca vm recipe doctor: %w", err))
	}
	return report, nil
}

func call[T any](ctx context.Context, c Client, args ...string) (T, error) {
	var envelope struct {
		Result T `json:"result"`
	}
	out, err := c.runner.Run(ctx, append(args, "--json")...)
	if err != nil {
		return envelope.Result, err
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return envelope.Result, fmt.Errorf("decode orca %s: %w", strings.Join(args, " "), err)
	}
	return envelope.Result, nil
}
