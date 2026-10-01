package tailnet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	launcherPath = remote.StateDir + "/tailscaled.sh"
	logPath      = remote.StateDir + "/tailscaled.log"
	socketWait   = 60
)

type Mode string

const (
	Kernel    Mode = "kernel"
	Userspace Mode = "userspace"
)

type Supervisor string

const (
	SpriteEnv Supervisor = "sprite-env"
	Setsid    Supervisor = "setsid"
)

type Daemon struct {
	Mode       Mode
	Supervisor Supervisor
}

type Node struct {
	NodeID  string `json:"nodeId"`
	IP      string `json:"ip"`
	DNSName string `json:"dnsName"`
	Mode    Mode   `json:"mode"`
}

func (d Daemon) State() string {
	switch d.Mode {
	case Kernel:
		return "/var/lib/tailscale/tailscaled.state"
	case Userspace:
		return remote.StateDir + "/tailscaled.state"
	}
	panic("unknown tailnet mode " + string(d.Mode))
}

func (d Daemon) socket() string {
	if d.Mode == Kernel {
		return "/run/tailscale/tailscaled.sock"
	}
	return remote.StateDir + "/tailscaled.sock"
}

func (d Daemon) cli() string {
	if d.Mode == Kernel {
		return "sudo -n tailscale --socket=" + d.socket()
	}
	return `tailscale --socket="` + d.socket() + `"`
}

func (d Daemon) upFlags() string {
	if d.Mode == Userspace {
		return " --accept-dns=false"
	}
	return ""
}

func (d Daemon) Launcher() string {
	if d.Mode == Kernel {
		return strings.Join([]string{
			"#!/bin/sh",
			"set -eu",
			"sudo -n mkdir -p /var/lib/tailscale /run/tailscale",
			"exec sudo -n tailscaled --state=" + d.State() + " --socket=" + d.socket(),
		}, "\n")
	}
	return strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		"exec tailscaled --tun=userspace-networking --state=" + d.State() + " --socket=" + d.socket(),
	}, "\n")
}

func (d Daemon) start() string {
	switch d.Supervisor {
	case SpriteEnv:
		return `sprite-env services get tailscaled >/dev/null 2>&1 || sprite-env services create tailscaled --cmd "` + launcherPath + `" --no-stream >&2`
	case Setsid:
		return `nohup setsid "` + launcherPath + `" >> "` + logPath + `" 2>&1 < /dev/null &`
	}
	panic("unknown tailnet supervisor " + string(d.Supervisor))
}

func (d Daemon) ensureRunning() string {
	return strings.Join([]string{
		`mkdir -p "` + remote.StateDir + `"`,
		`cat > "` + launcherPath + `" <<'SH'`,
		d.Launcher(),
		"SH",
		`chmod 700 "` + launcherPath + `"`,
		`if ! test -S "` + d.socket() + `"; then`,
		"  " + d.start(),
		"fi",
		"i=0",
		`until test -S "` + d.socket() + `"; do i=$((i + 1)); test "$i" -lt ` + fmt.Sprint(socketWait) + ` || { echo "` + remote.Prefix + `: tailscaled did not open ` + d.socket() + ` within 30s" >&2; exit 1; }; sleep 0.5; done`,
	}, "\n")
}

func (d Daemon) FreshScript() string {
	return remote.Script(`test ! -e "` + d.State() + `" || { echo "` + remote.Prefix + `: this machine already carries ` + d.State() + `, so its image or spare joined a tailnet before this claim; rebuild it unenrolled" >&2; exit 1; }`)
}

func (d Daemon) UpScript(hostname string) string {
	return remote.Script(
		"IFS= read -r key",
		"umask 077",
		`keyfile="$(mktemp)"`,
		`trap 'rm -f "$keyfile"' EXIT`,
		`printf '%s\n' "$key" > "$keyfile"`,
		d.cli()+` up --auth-key="file:$keyfile" --hostname=`+hostname+" --ssh --timeout=90s"+d.upFlags()+" >&2",
		d.cli()+" status --json",
	)
}

func (d Daemon) EnrollScript(hostname string) string {
	return remote.Script(d.ensureRunning(), d.UpScript(hostname))
}

func (d Daemon) StatusScript() string {
	return remote.Script(
		`test -e "`+d.State()+`" || { echo '{"BackendState":"NoState"}'; exit 0; }`,
		d.ensureRunning(),
		d.cli()+" status --json",
	)
}

func (d Daemon) LogoutScript() string {
	return remote.Script(
		`test -e "`+d.State()+`" || exit 0`,
		d.ensureRunning(),
		d.cli()+" logout",
	)
}

type status struct {
	BackendState string
	TUN          bool
	Self         struct {
		ID           string
		DNSName      string
		TailscaleIPs []string
	}
}

func ParseStatus(raw []byte, suffix string) (Node, bool, error) {
	var parsed status
	if err := json.Unmarshal(bytes.TrimSpace(raw), &parsed); err != nil {
		return Node{}, false, fmt.Errorf("tailscale status printed no JSON object: %w", err)
	}
	if parsed.BackendState != "Running" {
		return Node{NodeID: parsed.Self.ID}, false, nil
	}
	node := Node{NodeID: parsed.Self.ID, DNSName: strings.TrimSuffix(parsed.Self.DNSName, "."), Mode: Userspace}
	if parsed.TUN {
		node.Mode = Kernel
	}
	for _, raw := range parsed.Self.TailscaleIPs {
		if addr, err := netip.ParseAddr(raw); err == nil && addr.Is4() {
			node.IP = addr.String()
		}
	}
	if node.NodeID == "" || node.IP == "" || node.DNSName == "" {
		return Node{}, false, fmt.Errorf("tailscale status reports Running but no node id, IPv4 and MagicDNS name: %s", bytes.TrimSpace(raw))
	}
	if !strings.HasSuffix(node.DNSName, "."+suffix) {
		return Node{}, false, fmt.Errorf("node %s joined a tailnet other than the workspace tailnet: its MagicDNS name is %s", node.NodeID, node.DNSName)
	}
	return node, true, nil
}
