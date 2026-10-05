package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
	"github.com/yasyf/cc-remote/internal/remote"
	"github.com/yasyf/cc-remote/internal/version"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	helperReady   = "ready"
	helperArchive = "archive"
	helperAbsent  = "absent"
)

const helperProbeScript = `set -eu
store=` + images.HelperStore + `
digest="$1" binary="$2" admission="$3"
dir="$store/$digest"
for partial in "$store/$digest".*.partial; do
  if [ -e "$partial" ]; then
    echo "cc-remote: $partial is a partial bootstrap helper admission; it is never repaired or replaced" >&2
    exit 1
  fi
done
if [ ! -e "$dir" ] && [ ! -L "$dir" ]; then
  if [ -f "$store/$digest.tar.gz.admitted" ]; then echo ` + helperArchive + `; else echo ` + helperAbsent + `; fi
  exit 0
fi
file="$dir/cc-remote"
if [ -L "$dir" ] || [ ! -d "$dir" ] || [ -L "$file" ] || [ ! -f "$file" ] \
  || [ "$(stat -c '%u %a' "$dir")" != "0 755" ] || [ "$(stat -c '%u %a' "$file")" != "0 755" ] \
  || [ "$(cat "$dir/admission.json")" != "$admission" ]; then
  echo "cc-remote: $dir is not the admitted bootstrap helper; it is never repaired or replaced" >&2
  exit 1
fi
if ! sum="$(openssl dgst -sha256 -r "$file")"; then
  echo "cc-remote: openssl could not hash $file; it is never repaired or replaced" >&2
  exit 1
fi
got="${sum%% *}"
if [ "$got" != "$binary" ]; then
  echo "cc-remote: $file has sha256 $got, want $binary; it is never repaired or replaced" >&2
  exit 1
fi
echo ` + helperReady

const helperExtractScript = `set -euo pipefail
store=` + images.HelperStore + `
digest="$1" binary="$2" size="$3"
archive="$store/$digest.tar.gz.admitted"
if [ -e "$store/$digest" ] || [ -L "$store/$digest" ]; then
  echo "cc-remote: $store/$digest already exists; an admitted bootstrap helper is never replaced" >&2
  exit 1
fi
got="$(stat -c %s "$archive")"
if [ "$got" != "$size" ]; then
  echo "cc-remote: $archive is $got bytes, want $size" >&2
  exit 1
fi
if ! sum="$(openssl dgst -sha256 -r "$archive")"; then
  echo "cc-remote: openssl could not hash $archive" >&2
  exit 1
fi
got="${sum%% *}"
if [ "$got" != "$digest" ]; then
  echo "cc-remote: $archive has sha256 $got, want $digest" >&2
  exit 1
fi
count="$(tar -tzf "$archive" | grep -cx cc-remote || true)"
if [ "$count" != 1 ]; then
  echo "cc-remote: $archive holds $count cc-remote members, want exactly one" >&2
  exit 1
fi
mode="$(tar -tvzf "$archive" cc-remote | cut -c 1-10)"
if [ "$mode" != -rwxr-xr-x ]; then
  echo "cc-remote: $archive member cc-remote is $mode, want a regular -rwxr-xr-x file" >&2
  exit 1
fi
staging="$(mktemp -d "$store/$digest.XXXXXXXX.partial")"
chmod 0755 "$staging"
tar -xzOf "$archive" cc-remote > "$staging/cc-remote"
chmod 0755 "$staging/cc-remote"
if ! sum="$(openssl dgst -sha256 -r "$staging/cc-remote")"; then
  echo "cc-remote: openssl could not hash $staging/cc-remote" >&2
  exit 1
fi
got="${sum%% *}"
if [ "$got" != "$binary" ]; then
  echo "cc-remote: $archive member cc-remote has sha256 $got, want $binary" >&2
  exit 1
fi
printf '%s\n' "$staging"`

const helperPromoteScript = `set -euo pipefail
store=` + images.HelperStore + `
digest="$1" staging="$2" admission="$3"
case "$staging" in
"$store/$digest".*.partial) ;;
*)
  echo "cc-remote: $staging is not a staged bootstrap helper" >&2
  exit 1
  ;;
esac
printf '%s\n' "$admission" > "$staging/admission.json"
chmod 0644 "$staging/admission.json"
(
  flock 9
  if [ -e "$store/$digest" ] || [ -L "$store/$digest" ]; then
    echo "cc-remote: $store/$digest already exists; an admitted bootstrap helper is never replaced" >&2
    exit 1
  fi
  mv -T "$staging" "$store/$digest"
) 9> "$store/.lock"`

type helperAdmission struct {
	Schema       int    `json:"schema"`
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	BinarySHA256 string `json:"binarySha256"`
}

func selectHelper(session *workspace.Session) (*config.BootstrapHelper, error) {
	helper := session.Config.Orca.BootstrapHelper
	if helper == nil || session.Kind != sprites.Name {
		return nil, nil
	}
	if running := version.String(); helper.Version != running {
		return nil, fmt.Errorf("orca.bootstrap_helper pins cc-remote %s, and this is cc-remote %s; the guest helper must be the running release", helper.Version, running)
	}
	return helper, nil
}

func helperPath(helper *config.BootstrapHelper) string {
	return images.HelperStore + "/" + helper.SHA256 + "/cc-remote"
}

func admissionOf(helper *config.BootstrapHelper) string {
	raw, _ := json.Marshal(helperAdmission{Schema: 1, Version: helper.Version, Platform: helper.Platform, SHA256: helper.SHA256, Size: helper.Size, BinarySHA256: helper.BinarySHA256})
	return string(raw)
}

func guestExec(session *workspace.Session, machine, step string) func(context.Context, []string) ([]byte, error) {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		result, err := session.Provider.Exec(ctx, machine, argv, nil)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, &providers.CommandError{Command: step + " on " + machine, Result: result}
		}
		return result.Stdout, nil
	}
}

func probeHelper(ctx context.Context, session *workspace.Session, machine string, helper *config.BootstrapHelper) (string, error) {
	out, err := guestExec(session, machine, "the bootstrap helper check")(ctx, []string{"sh", "-c", helperProbeScript, "probe-helper", helper.SHA256, helper.BinarySHA256, admissionOf(helper)})
	if err != nil {
		return "", err
	}
	switch state := strings.TrimSpace(string(out)); state {
	case helperReady, helperArchive, helperAbsent:
		return state, nil
	default:
		return "", fmt.Errorf("the bootstrap helper check on %s printed %q", machine, state)
	}
}

func ensureHelper(ctx context.Context, session *workspace.Session, machine string, helper *config.BootstrapHelper) error {
	state, err := probeHelper(ctx, session, machine, helper)
	if err != nil || state == helperReady {
		return err
	}
	if state == helperAbsent {
		if err := session.FetchHelper(ctx, machine); err != nil {
			return err
		}
	}
	out, err := guestExec(session, machine, "the bootstrap helper extraction")(ctx, []string{"sudo", "bash", "-c", helperExtractScript, "extract-helper", helper.SHA256, helper.BinarySHA256, strconv.FormatInt(helper.Size, 10)})
	if err != nil {
		return err
	}
	staging := strings.TrimSpace(string(out))
	printed, err := guestExec(session, machine, "the staged bootstrap helper")(ctx, []string{"sh", "-c", `exec "$1" version 2>&1`, "helper-version", staging + "/cc-remote"})
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(printed)); got != helper.Version {
		return fmt.Errorf("the staged bootstrap helper on %s is cc-remote %q, want %s; %s stays as a partial admission", machine, got, helper.Version, staging)
	}
	if _, err := guestExec(session, machine, "the bootstrap helper admission")(ctx, []string{"sudo", "bash", "-c", helperPromoteScript, "promote-helper", helper.SHA256, staging, admissionOf(helper)}); err != nil {
		return err
	}
	return verifyHelper(ctx, session, machine, helper)
}

func verifyHelper(ctx context.Context, session *workspace.Session, machine string, helper *config.BootstrapHelper) error {
	state, err := probeHelper(ctx, session, machine, helper)
	if err == nil && state != helperReady {
		return fmt.Errorf("%s has no admitted bootstrap helper %s (%s); a claimed spare is never prepared", machine, helper.SHA256[:12], state)
	}
	return err
}

func claimHelper(ctx context.Context, session *workspace.Session, machine string) error {
	helper, err := selectHelper(session)
	if err != nil || helper == nil {
		return err
	}
	return verifyHelper(ctx, session, machine, helper)
}

func (d orcaDriver) prepareHelper(ctx context.Context, session *workspace.Session, task *orcaTask) (string, error) {
	helper, err := selectHelper(session)
	if err != nil || helper == nil {
		return "", err
	}
	started := time.Now()
	if task.claimed {
		err = verifyHelper(ctx, session, task.Machine, helper)
	} else {
		err = ensureHelper(ctx, session, task.Machine, helper)
	}
	d.observe("helper.prepare", started, err)
	return helperPath(helper), err
}

func (d orcaDriver) guestBootstrap(ctx context.Context, task *orcaTask, helper string, runtime orca.Runtime, startup orca.Startup, trusted bool, p orca.Poll) ([]string, error) {
	descriptor, err := json.Marshal(orca.NewGuest(config.HelperPlatform, task.RuntimeID, task.Terminal, path.Join(path.Dir(runtime.Entry), orca.GuestCLI), task.Agent, startup, trusted, p))
	if err != nil {
		return nil, err
	}
	started := time.Now()
	out, err := task.run(ctx, "exec "+remote.Quote(helper)+" orca guest-bootstrap", bytes.NewReader(descriptor))
	var steps []string
	if err == nil {
		steps, err = orca.ParseGuestResult(out)
	}
	d.observe("bootstrap.guest", started, err)
	return steps, err
}

func newOrcaGuestBootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guest-bootstrap",
		Short: "Answer a new worker's startup screens through the workspace's own Orca CLI",
		Long: `guest-bootstrap runs on the workspace, started once by orca create and prepare after the worker's key
is delivered. It reads one descriptor on stdin naming the runtime, terminal, agent, and hook checks, drives the
same startup state machine through the installed Orca CLI and its local runtime, and prints one result.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var guest orca.Guest
			decoder := json.NewDecoder(cmd.InOrStdin())
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&guest); err != nil {
				return fmt.Errorf("read the guest bootstrap descriptor: %w", err)
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			steps, err := guest.Run(cmd.Context(), home, orca.LocalExec)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(orca.GuestOutcome(steps, err))
		},
	}
}
