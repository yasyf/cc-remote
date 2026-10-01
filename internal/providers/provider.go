package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

type Provider interface {
	Check(ctx context.Context) error
	Rate(spec Spec) (Rate, error)
	Create(ctx context.Context, spec Spec) (Machine, error)
	Get(ctx context.Context, id string) (Machine, error)
	Wake(ctx context.Context, id string) error
	Suspend(ctx context.Context, id string) error
	Destroy(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (Result, error)
	SSHTarget(ctx context.Context, id string) (Target, error)
	List(ctx context.Context, labels map[string]string) ([]Machine, error)
}

var (
	ErrNotFound = errors.New("machine not found")
	ErrExists   = errors.New("machine already exists")
)

type Spec struct {
	Name    string
	Profile string
	Image   string
	Size    string
	Region  string
	Labels  map[string]string
}

type State string

const (
	StateRunning   State = "running"
	StateSuspended State = "suspended"
	StateUnknown   State = "unknown"
)

type Machine struct {
	ID        string
	Provider  string
	State     State
	CreatedAt time.Time
	Labels    map[string]string
}

func (m Machine) HasLabels(labels map[string]string) bool {
	for key, value := range labels {
		if have, ok := m.Labels[key]; !ok || have != value {
			return false
		}
	}
	return true
}

type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Rate struct {
	HourlyUSD         float64
	StorageGB         float64
	StorageGBMonthUSD float64
}

type HostKeyMode string

const (
	HostKeyPinned       HostKeyMode = "pinned"
	HostKeyProxyTrusted HostKeyMode = "proxy-trusted"
)

type HostKeyPolicy struct {
	Mode           HostKeyMode
	Alias          string
	KnownHostsFile string
}

type Target struct {
	Host          string
	Port          int
	User          string
	IdentityFile  string
	ProxyCommand  string
	HostKeyPolicy HostKeyPolicy
}

func (t Target) SSHOptions() []string {
	options := []string{
		"HostName=" + t.Host,
		"Port=" + strconv.Itoa(t.Port),
		"User=" + t.User,
		"IdentityFile=" + t.IdentityFile,
		"IdentitiesOnly=yes",
		"ProxyCommand=" + t.ProxyCommand,
	}
	switch t.HostKeyPolicy.Mode {
	case HostKeyPinned:
		return append(options,
			"HostKeyAlias="+t.HostKeyPolicy.Alias,
			"UserKnownHostsFile="+t.HostKeyPolicy.KnownHostsFile,
			"StrictHostKeyChecking=yes",
		)
	case HostKeyProxyTrusted:
		return append(options, "UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=no")
	}
	panic(fmt.Sprintf("host key mode %q", t.HostKeyPolicy.Mode))
}
