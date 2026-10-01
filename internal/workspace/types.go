package workspace

import (
	"net"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/tailnet"
)

const SchemaVersion = 1

type Forward struct {
	Label string `json:"label"`
	Port  int    `json:"port"`
}

type LabelledEnv struct {
	Label string
	Env   string
}

type Platform struct {
	Daemon           tailnet.Daemon
	HostKeys         bool
	CredentialHelper string
}

type Record struct {
	Name      string        `json:"name"`
	Provider  string        `json:"provider"`
	Profile   string        `json:"profile"`
	Source    Source        `json:"source"`
	Machine   string        `json:"machine"`
	Claimed   bool          `json:"claimed,omitempty"`
	Ready     bool          `json:"ready,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Forwards  []Forward     `json:"forwards,omitempty"`
	Tailnet   *tailnet.Node `json:"tailnet,omitempty"`
}

type SSH struct {
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	IdentityFile   string   `json:"identityFile,omitempty"`
	ProxyCommand   string   `json:"proxyCommand,omitempty"`
	HostKeyAlias   string   `json:"hostKeyAlias,omitempty"`
	KnownHostsFile string   `json:"knownHostsFile,omitempty"`
	Options        []string `json:"options"`
}

type Result struct {
	SchemaVersion int           `json:"schemaVersion"`
	Name          string        `json:"name"`
	Provider      string        `json:"provider"`
	Profile       string        `json:"profile"`
	Source        Source        `json:"source"`
	Machine       string        `json:"machine"`
	ProjectRoot   string        `json:"projectRoot"`
	SSH           SSH           `json:"ssh"`
	Forwards      []Forward     `json:"forwards,omitempty"`
	Tailnet       *tailnet.Node `json:"tailnet,omitempty"`
}

func sshFromTarget(target providers.Target) SSH {
	return SSH{
		Host:           target.Host,
		Port:           target.Port,
		User:           target.User,
		IdentityFile:   target.IdentityFile,
		ProxyCommand:   target.ProxyCommand,
		HostKeyAlias:   target.HostKeyPolicy.Alias,
		KnownHostsFile: target.HostKeyPolicy.KnownHostsFile,
		Options:        target.SSHOptions(),
	}
}

func FreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	return listener.Addr().(*net.TCPAddr).Port, listener.Close()
}

func AllocateForwards(labels []LabelledEnv, held []Forward) ([]Forward, error) {
	ports := map[string]int{}
	taken := map[int]bool{}
	for _, forward := range held {
		ports[forward.Label] = forward.Port
		taken[forward.Port] = true
	}
	forwards := make([]Forward, 0, len(labels))
	for _, label := range labels {
		port, ok := ports[label.Label]
		if !ok {
			var err error
			if port, err = freePortOutside(taken); err != nil {
				return nil, err
			}
			taken[port] = true
		}
		forwards = append(forwards, Forward{Label: label.Label, Port: port})
	}
	return forwards, nil
}

func freePortOutside(taken map[int]bool) (int, error) {
	for {
		port, err := FreePort()
		if err != nil || !taken[port] {
			return port, err
		}
	}
}
