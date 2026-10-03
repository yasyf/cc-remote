package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers/namespace"
	"github.com/yasyf/cc-remote/internal/state"
)

const (
	gatewayLockTries = 20
	gatewayLockWait  = 10 * time.Millisecond
)

type gatewayKeeper interface {
	Forward(ctx context.Context, id string, listener net.Listener) error
	Keep(ctx context.Context, id string, observe func(namespace.Lease)) error
}

func newOrcaForwardCmd() *cobra.Command {
	var flags selection
	cmd := &cobra.Command{
		Use:   "forward <name>",
		Short: "Hold a Compute task's loopback listener and renew its instance lease",
		Long: `forward runs detached, started by orca create, prepare, and reconnect. It binds the task's recorded
127.0.0.1 port, takes the task's gateway lock, bridges each local connection to the instance's exported
runtime port through the Namespace gateway, and renews the instance's lease, running or suspended, from the
deadline the provider last answered. A failed, unchanged, or short renewal is recorded with that deadline, and
the forward exits once the instance is destroyed. A port already in use is reported, never replaced by another port.`,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			task, err := openTask(cfg, args[0])
			if err != nil {
				return err
			}
			if task.Gateway == nil {
				return fmt.Errorf("%s is a %s task forwarded over SSH, not a Compute gateway task", task.Workspace, task.Provider)
			}
			keeper, ok := task.provider.(gatewayKeeper)
			if !ok {
				return fmt.Errorf("provider %s has no gateway forward", task.Provider)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return forward(ctx, cfg.State(), task, keeper)
		},
	}
	flags.bind(cmd)
	return cmd
}

func forward(ctx context.Context, dir state.Dir, task *orcaTask, keeper gatewayKeeper) error {
	address := net.JoinHostPort(orca.Loopback, strconv.Itoa(task.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("hold %s for %s: %w; the recorded port stays and no other process is stopped", address, task.Workspace, err)
	}
	unlock, err := holdGateway(task.Gateway.Lock)
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	defer unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	kept, served := make(chan error, 1), make(chan error, 1)
	go func() {
		kept <- keeper.Keep(ctx, task.Gateway.Instance, func(lease namespace.Lease) {
			if err := state.Save(dir.OrcaLease(task.Workspace, task.Gateway.Instance), lease); err != nil {
				slog.Error("recording the lease failed", "workspace", task.Workspace, "err", err)
			}
		})
	}()
	go func() { served <- keeper.Forward(ctx, task.Gateway.Instance, listener) }()
	var first, second error
	select {
	case first = <-kept:
		cancel()
		second = <-served
	case first = <-served:
		cancel()
		second = <-kept
	}
	if errors.Is(second, context.Canceled) {
		second = nil
	}
	if errors.Is(first, context.Canceled) {
		first = nil
	}
	return errors.Join(first, second)
}

func holdGateway(path string) (func(), error) {
	for range gatewayLockTries {
		unlock, held, err := state.TryLock(path)
		if err != nil || held {
			return unlock, err
		}
		// An up() probe holds the lock for an instant; only another forward holds it for longer.
		time.Sleep(gatewayLockWait)
	}
	return nil, fmt.Errorf("another forward already holds %s", path)
}

func lastLine(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	var last string
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		if line := strings.TrimSpace(lines.Text()); line != "" {
			last = line
		}
	}
	return last
}
