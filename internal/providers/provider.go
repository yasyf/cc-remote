package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

type Provider interface {
	Traits() Traits
	Check(ctx context.Context) error
	ValidateSpec(spec Spec) error
	Create(ctx context.Context, spec Spec) (Machine, error)
	Get(ctx context.Context, id string) (Machine, error)
	Wake(ctx context.Context, id string) error
	Suspend(ctx context.Context, id string) error
	Destroy(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, cmd []string, stdin io.Reader) (Result, error)
	Access(ctx context.Context, id string) (Access, error)
	List(ctx context.Context, labels map[string]string) ([]Machine, error)
}

type Extender interface {
	Extend(ctx context.Context, id string, by time.Duration) (time.Time, error)
}

var (
	ErrNotFound  = errors.New("machine not found")
	ErrExists    = errors.New("machine already exists")
	ErrAmbiguous = errors.New("the create failed after the machine came to exist")
)

type TailnetMode string

const (
	TailnetKernel    TailnetMode = "kernel"
	TailnetUserspace TailnetMode = "userspace"
)

type Supervisor string

const (
	SupervisorSpriteEnv Supervisor = "sprite-env"
	SupervisorSetsid    Supervisor = "setsid"
)

type Traits struct {
	TailnetMode TailnetMode
	Supervisor  Supervisor
	Platform    string
}

type Spec struct {
	Name      string
	Profile   string
	Image     string
	Size      string
	Region    string
	Root      string
	Home      string
	Labels    map[string]string
	Allocated func(ctx context.Context, machine Machine) error
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
	Compute   *ComputeInstance
}

type ComputeInstance struct {
	InstanceID    string    `json:"instanceId"`
	Container     string    `json:"container"`
	Region        string    `json:"region,omitempty"`
	Endpoint      string    `json:"endpoint"`
	Image         string    `json:"image"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	Deadline      time.Time `json:"deadline"`
	Volume        Volume    `json:"volume"`
	ContainerPort int       `json:"containerPort"`
	ExportedPort  int       `json:"exportedPort,omitempty"`
	IngressDomain string    `json:"ingressDomain,omitempty"`
}

type Volume struct {
	Tag            string `json:"tag"`
	HostMount      string `json:"hostMount"`
	ContainerMount string `json:"containerMount"`
}

type AccessKind string

const (
	AccessOpenSSH AccessKind = "openssh"
	AccessCompute AccessKind = "compute"
)

type Access struct {
	Kind    AccessKind
	SSH     Target
	Compute ComputeInstance
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
	options := []string{"HostName=" + t.Host, "Port=" + strconv.Itoa(t.Port), "User=" + t.User}
	if t.IdentityFile != "" {
		options = append(options, "IdentityFile="+sshPath(t.IdentityFile), "IdentitiesOnly=yes")
	}
	if t.ProxyCommand != "" {
		options = append(options, "ProxyCommand="+sshTokens(t.ProxyCommand))
	}
	switch t.HostKeyPolicy.Mode {
	case HostKeyPinned:
		return append(options,
			"HostKeyAlias="+t.HostKeyPolicy.Alias,
			"UserKnownHostsFile="+sshPath(t.HostKeyPolicy.KnownHostsFile),
			"StrictHostKeyChecking=yes",
		)
	case HostKeyProxyTrusted:
		return append(options, "UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=no")
	}
	panic(fmt.Sprintf("host key mode %q", t.HostKeyPolicy.Mode))
}

func sshTokens(value string) string { return strings.ReplaceAll(value, "%", "%%") }

func sshPath(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(sshTokens(path)) + `"`
}
