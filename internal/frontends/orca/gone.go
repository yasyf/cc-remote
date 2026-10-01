package orca

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

type Removal struct {
	Host string `json:"host"`
	Gone bool   `json:"gone"`
}

func (c Client) Gone(ctx context.Context, hostID string) (Removal, error) {
	rows, err := c.Worktrees(ctx)
	if err != nil {
		return Removal{}, err
	}
	if slices.ContainsFunc(rows, func(w Worktree) bool { return w.HostID == hostID }) {
		return Removal{}, fmt.Errorf("orca still lists a workspace on %s", hostID)
	}
	hosts, err := c.Hosts(ctx)
	if err != nil {
		return Removal{}, err
	}
	id := strings.TrimPrefix(hostID, sshHostPrefix)
	if slices.ContainsFunc(hosts, func(h Host) bool { return h.ID == id }) {
		return Removal{}, fmt.Errorf("orca still lists SSH target %s", hostID)
	}
	return Removal{Host: hostID, Gone: true}, nil
}
