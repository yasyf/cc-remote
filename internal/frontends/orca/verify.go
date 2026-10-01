package orca

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	probeTitle   = "remote-check"
	ProbeCommand = `branch=$(git rev-parse --abbrev-ref HEAD) && echo "remote-check $(hostname) $branch $PWD"; echo "remote-check-exit $?"`
)

var (
	ErrProbeRunning = errors.New("remote-check has not exited")
	probeExitLine   = regexp.MustCompile(`^remote-check-exit ([0-9]+)$`)
	probeResultLine = regexp.MustCompile(`^remote-check ([^$ ]+) ([^ ]+) (/.*)$`)
)

type Probe struct {
	Hostname string
	Branch   string
	Cwd      string
}

type ProbeExitError struct {
	Status int
}

type VerifyOptions struct {
	Workspace     string
	RepoID        string
	Open          string
	LocalHostname string
}

type Verification struct {
	Worktree      string `json:"worktree"`
	Host          string `json:"host"`
	Target        string `json:"target"`
	Connected     bool   `json:"connected"`
	Hostname      string `json:"hostname"`
	Branch        string `json:"branch"`
	Cwd           string `json:"cwd"`
	Editor        string `json:"editor"`
	ProbeTerminal string `json:"probeTerminal"`
}

func (e ProbeExitError) Error() string {
	return fmt.Sprintf("remote-check exited %d", e.Status)
}

func ParseProbe(tail []string) (Probe, error) {
	status := -1
	var probe *Probe
	for _, raw := range tail {
		line := strings.TrimRight(raw, " \r")
		if m := probeExitLine.FindStringSubmatch(line); m != nil && status < 0 {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return Probe{}, fmt.Errorf("remote-check exit status %q: %w", m[1], err)
			}
			status = n
		}
		if m := probeResultLine.FindStringSubmatch(line); m != nil && probe == nil {
			probe = &Probe{Hostname: m[1], Branch: m[2], Cwd: m[3]}
		}
	}
	switch {
	case status < 0:
		return Probe{}, ErrProbeRunning
	case status != 0:
		return Probe{}, ProbeExitError{Status: status}
	case probe == nil:
		return Probe{}, errors.New("remote-check exited 0 without its result line")
	}
	return *probe, nil
}

func (c Client) Verify(ctx context.Context, o VerifyOptions, wait Poll) (Verification, error) {
	rows, err := c.sshWorktrees(ctx, o.Workspace, o.RepoID)
	if err != nil {
		return Verification{}, err
	}
	switch len(rows) {
	case 0:
		return Verification{}, fmt.Errorf("no SSH workspace named %s in Orca", o.Workspace)
	case 1:
	default:
		return Verification{}, fmt.Errorf("%d SSH workspaces named %s in Orca; pass --repo", len(rows), o.Workspace)
	}
	row := rows[0]
	target, err := c.sshHost(ctx, row.HostID)
	if err != nil {
		return Verification{}, err
	}
	if !target.Connected {
		return Verification{}, fmt.Errorf("SSH target %s (%s) is not connected", row.HostID, target.Name)
	}
	if err := c.OpenFile(ctx, row.ID, o.Open); err != nil {
		return Verification{}, err
	}
	terminal, err := c.CreateTerminal(ctx, row.ID, probeTitle, ProbeCommand)
	if err != nil {
		return Verification{}, err
	}
	if terminal.ExecutionHostID != row.HostID {
		return Verification{}, fmt.Errorf("the remote-check terminal %s started on %q, not %s", terminal.Handle, terminal.ExecutionHostID, row.HostID)
	}
	probe, err := poll(ctx, wait, func(ctx context.Context) (Probe, bool, error) {
		read, err := c.ReadTerminal(ctx, terminal.Handle)
		if err != nil {
			return Probe{}, false, err
		}
		probe, err := ParseProbe(read.Tail)
		if errors.Is(err, ErrProbeRunning) {
			return Probe{}, false, nil
		}
		return probe, true, err
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return Verification{}, fmt.Errorf("the remote-check command in terminal %s never exited: %w", terminal.Handle, err)
	}
	if err != nil {
		return Verification{}, fmt.Errorf("terminal %s: %w", terminal.Handle, err)
	}
	if probe.Hostname == o.LocalHostname {
		return Verification{}, fmt.Errorf("remote-check in terminal %s ran on this Mac, not %s", terminal.Handle, row.HostID)
	}
	return Verification{
		Worktree:      row.ID,
		Host:          row.HostID,
		Target:        target.Name,
		Connected:     target.Connected,
		Hostname:      probe.Hostname,
		Branch:        probe.Branch,
		Cwd:           probe.Cwd,
		Editor:        o.Open + " opened",
		ProbeTerminal: terminal.Handle,
	}, nil
}

func (c Client) sshHost(ctx context.Context, hostID string) (Host, error) {
	hosts, err := c.Hosts(ctx)
	if err != nil {
		return Host{}, err
	}
	id := strings.TrimPrefix(hostID, sshHostPrefix)
	for _, h := range hosts {
		if h.ID == id {
			return h, nil
		}
	}
	return Host{}, fmt.Errorf("no SSH target %s in Orca", hostID)
}
