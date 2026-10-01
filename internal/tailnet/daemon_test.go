package tailnet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const runningStatus = `{"BackendState":"Running","TUN":true,"Self":{"ID":"nNODE","HostName":"ws-1","DNSName":"ws-1.example.ts.net.","TailscaleIPs":["fd7a:115c:a1e0::1","100.64.0.9"]}}`

var (
	sprites   = Daemon{Mode: Kernel, Supervisor: SpriteEnv}
	namespace = Daemon{Mode: Userspace, Supervisor: Setsid}
	daemons   = []Daemon{sprites, namespace}
)

func TestParseStatusReadsTheNodeIdentityAndMode(t *testing.T) {
	node, running, err := ParseStatus([]byte(runningStatus+"\n"), suffix)
	if err != nil || !running {
		t.Fatalf("running = %v, err = %v", running, err)
	}
	if want := (Node{NodeID: "nNODE", IP: "100.64.0.9", DNSName: "ws-1.example.ts.net", Mode: Kernel}); node != want {
		t.Errorf("node = %+v", node)
	}
	userspace, _, err := ParseStatus([]byte(strings.Replace(runningStatus, `"TUN":true`, `"TUN":false`, 1)), suffix)
	if err != nil || userspace.Mode != Userspace {
		t.Errorf("userspace node = %+v, err = %v", userspace, err)
	}
}

func TestParseStatusRefusesANodeOnAnotherTailnet(t *testing.T) {
	if _, _, err := ParseStatus([]byte(runningStatus), "other.ts.net"); err == nil || !strings.Contains(err.Error(), "other than the workspace tailnet") {
		t.Errorf("err = %v", err)
	}
}

func TestParseStatusReportsAMachineOffTheTailnet(t *testing.T) {
	for _, raw := range []string{`{"BackendState":"NeedsLogin","Self":{"ID":"nOLD"}}`, `{"BackendState":"NoState"}`} {
		if _, running, err := ParseStatus([]byte(raw), suffix); err != nil || running {
			t.Errorf("%s: running = %v, err = %v", raw, running, err)
		}
	}
	for _, raw := range []string{"Logged out.", `{"BackendState":"Running","Self":{"ID":"nNODE"}}`} {
		if _, _, err := ParseStatus([]byte(raw), suffix); err == nil {
			t.Errorf("%s parsed as a node", raw)
		}
	}
}

func fakeBinary(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestUpScriptNeverPutsTheKeyInArgvOrLeavesItOnDisk(t *testing.T) {
	bin := t.TempDir()
	seen := filepath.Join(t.TempDir(), "seen")
	fakeBinary(t, bin, "sudo", `[ "$1" = "-n" ] || exit 1
shift
exec "$@"`)
	fakeBinary(t, bin, "tailscale", `printf '%s\n' "$@" >> "`+seen+`.argv"
for arg in "$@"; do
  case "$arg" in
    --auth-key=file:*) cat "${arg#--auth-key=file:}" > "`+seen+`.key"; printf '%s\n' "${arg#--auth-key=file:}" > "`+seen+`.path" ;;
  esac
done
[ "$2" = status ] && echo '`+runningStatus+`'
exit 0`)
	for _, daemon := range daemons {
		for _, name := range []string{".argv", ".key", ".path"} {
			_ = os.Remove(seen + name)
		}
		cmd := exec.Command("sh", "-c", daemon.UpScript("ws-1"))
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader("tskey-auth-kNEW-minted\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v", daemon, err)
		}
		read := func(name string) string {
			raw, err := os.ReadFile(seen + name)
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(raw))
		}
		if argv := read(".argv"); strings.Contains(argv, "tskey-auth") || !strings.Contains(argv, "--hostname=ws-1") || !strings.Contains(argv, "--auth-key=file:") || !strings.Contains(argv, "--ssh") {
			t.Errorf("%v: tailscale saw argv %q", daemon, argv)
		}
		if key := read(".key"); key != "tskey-auth-kNEW-minted" {
			t.Errorf("%v: tailscale read key %q", daemon, key)
		}
		if _, err := os.Stat(read(".path")); err == nil {
			t.Errorf("%v: the key file outlived the enrollment", daemon)
		}
		node, running, err := ParseStatus(out, suffix)
		if err != nil || !running || node.NodeID != "nNODE" {
			t.Errorf("%v: enrollment printed %s: %v", daemon, out, err)
		}
	}
}

func TestKernelModeRunsTailscaledAsRootUnderSpriteEnv(t *testing.T) {
	script := sprites.EnrollScript("ws-1")
	if !strings.Contains(script, `sprite-env services create tailscaled --cmd "`+launcherPath+`"`) || strings.Contains(script, "setsid") {
		t.Errorf("enrollment starts tailscaled outside sprite-env:\n%s", script)
	}
	if launcher := sprites.Launcher(); !strings.Contains(launcher, "exec sudo -n tailscaled --state=/var/lib/tailscale/tailscaled.state") || strings.Contains(launcher, "userspace") {
		t.Errorf("launcher = %q", launcher)
	}
	if !strings.Contains(script, "sudo -n tailscale --socket=/run/tailscale/tailscaled.sock up ") || strings.Contains(script, "accept-dns") {
		t.Errorf("enrollment does not drive the root daemon:\n%s", script)
	}
}

func TestUserspaceModeRunsTailscaledUnprivilegedUnderSetsid(t *testing.T) {
	script := namespace.EnrollScript("ws-1")
	if !strings.Contains(script, `nohup setsid "`+launcherPath+`"`) || strings.Contains(script, "sprite-env") || strings.Contains(script, "sudo") {
		t.Errorf("enrollment needs a supervisor or root the machine lacks:\n%s", script)
	}
	if launcher := namespace.Launcher(); !strings.Contains(launcher, "exec tailscaled --tun=userspace-networking --state=$HOME/.cc-remote/tailscaled.state --socket=$HOME/.cc-remote/tailscaled.sock") {
		t.Errorf("launcher = %q", launcher)
	}
	if !strings.Contains(script, " up ") || !strings.Contains(script, "--accept-dns=false") {
		t.Errorf("enrollment lets an unprivileged daemon rewrite the OS resolver:\n%s", script)
	}
}

func TestScriptsParseAsPOSIXShell(t *testing.T) {
	for _, daemon := range daemons {
		for name, script := range map[string]string{
			"fresh": daemon.FreshScript(), "enroll": daemon.EnrollScript("ws-1"),
			"status": daemon.StatusScript(), "logout": daemon.LogoutScript(), "launcher": daemon.Launcher(),
		} {
			if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("%v %s script: %v: %s", daemon, name, err, out)
			}
		}
	}
}

func TestScriptsKeepTheStateTheNextBootReattachesWith(t *testing.T) {
	for _, daemon := range daemons {
		for _, script := range []string{daemon.EnrollScript("ws-1"), daemon.StatusScript(), daemon.LogoutScript()} {
			if !strings.Contains(script, daemon.Launcher()) || !strings.Contains(script, "--state="+daemon.State()) {
				t.Errorf("%v script starts tailscaled without the persistent state the next boot re-attaches with:\n%s", daemon, script)
			}
		}
		if fresh := daemon.FreshScript(); !strings.Contains(fresh, `test ! -e "`+daemon.State()+`"`) || strings.Contains(fresh, "tailscaled ") {
			t.Errorf("%v claim does not refuse a machine that already carries tailscaled state before any daemon starts:\n%s", daemon, fresh)
		}
		if strings.Contains(daemon.StatusScript(), " up ") || strings.Contains(daemon.LogoutScript(), " up ") {
			t.Errorf("%v status or logout script enrolls", daemon)
		}
	}
}
