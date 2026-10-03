package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers/namespace"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

type fakeKeeper struct {
	keep    func(ctx context.Context, observe func(namespace.Lease)) error
	forward func(ctx context.Context, listener net.Listener) error
}

func (k fakeKeeper) Keep(ctx context.Context, _ string, observe func(namespace.Lease)) error {
	return k.keep(ctx, observe)
}

func (k fakeKeeper) Forward(ctx context.Context, _ string, listener net.Listener) error {
	return k.forward(ctx, listener)
}

func untilCanceled(ctx context.Context, _ func(namespace.Lease)) error {
	<-ctx.Done()
	return ctx.Err()
}

func serveUntilCanceled(ctx context.Context, listener net.Listener) error {
	<-ctx.Done()
	return errors.Join(ctx.Err(), listener.Close())
}

func gatewayTask(t *testing.T, dir state.Dir) *orcaTask {
	t.Helper()
	port, err := workspace.FreePort()
	if err != nil {
		t.Fatal(err)
	}
	return &orcaTask{Workspace: "task-a", Provider: "namespace", Machine: "inst1", Port: port, Gateway: &orcaGateway{
		Instance:      "inst1",
		ContainerPort: 18766,
		Lock:          dir.OrcaGatewayLock("task-a", "inst1"),
		Log:           dir.OrcaForwardLog("task-a"),
	}}
}

func lockFree(t *testing.T, path string) bool {
	t.Helper()
	unlock, free, err := state.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if free {
		unlock()
	}
	return free
}

func TestForwardReportsAHeldPortAndPicksNoOther(t *testing.T) {
	dir := state.Dir(t.TempDir())
	task := gatewayTask(t, dir)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(task.Port))
	occupied, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	keeper := fakeKeeper{keep: func(context.Context, func(namespace.Lease)) error {
		t.Error("the keeper ran without the listener")
		return nil
	}}
	err = forward(t.Context(), dir, task, keeper)
	var bind *net.OpError
	if err == nil || !strings.HasPrefix(err.Error(), "hold "+address+" for task-a: ") || !strings.HasSuffix(err.Error(), "; the recorded port stays and no other process is stopped") || !errors.As(err, &bind) {
		t.Fatalf("forward = %v, want the bind failure on the recorded port", err)
	}
	if !lockFree(t, task.Gateway.Lock) {
		t.Error("a failed bind left the gateway lock held")
	}
}

func TestForwardRefusesWhileAnotherForwardHoldsTheLock(t *testing.T) {
	dir := state.Dir(t.TempDir())
	task := gatewayTask(t, dir)
	unlock, held, err := state.TryLock(task.Gateway.Lock)
	if err != nil || !held {
		t.Fatalf("TryLock = %t, %v", held, err)
	}
	defer unlock()
	keeper := fakeKeeper{keep: untilCanceled, forward: serveUntilCanceled}
	if err := forward(t.Context(), dir, task, keeper); err == nil || err.Error() != "another forward already holds "+task.Gateway.Lock {
		t.Errorf("forward = %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(task.Port)))
	if err != nil {
		t.Fatalf("the refused forward kept its port: %v", err)
	}
	_ = listener.Close()
}

func TestForwardRecordsEachLeaseAndStopsWithTheInstance(t *testing.T) {
	dir := state.Dir(t.TempDir())
	task := gatewayTask(t, dir)
	lease := namespace.Lease{InstanceID: "inst1", Status: "DESTROYED", Deadline: time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC), Horizon: "4h0m0s", Stopped: true}
	served := make(chan int, 1)
	keeper := fakeKeeper{
		keep: func(_ context.Context, observe func(namespace.Lease)) error {
			if lockFree(t, task.Gateway.Lock) {
				t.Error("the keeper ran without the gateway lock")
			}
			observe(lease)
			return nil
		},
		forward: func(ctx context.Context, listener net.Listener) error {
			served <- listener.Addr().(*net.TCPAddr).Port
			return serveUntilCanceled(ctx, listener)
		},
	}
	if err := forward(t.Context(), dir, task, keeper); err != nil {
		t.Fatalf("forward = %v, want a clean stop once the instance is gone", err)
	}
	if port := <-served; port != task.Port {
		t.Errorf("served on %d, want the recorded port %d", port, task.Port)
	}
	raw, err := os.ReadFile(dir.OrcaLease("task-a", "inst1"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded namespace.Lease
	if err := json.Unmarshal(raw, &recorded); err != nil || recorded != lease {
		t.Errorf("recorded lease = %+v, %v; want %+v", recorded, err, lease)
	}
	if !lockFree(t, task.Gateway.Lock) {
		t.Error("the stopped forward kept its gateway lock")
	}
}

func TestForwardReturnsAListenerFailure(t *testing.T) {
	dir := state.Dir(t.TempDir())
	task := gatewayTask(t, dir)
	sentinel := errors.New("accept: too many open files")
	keeper := fakeKeeper{keep: untilCanceled, forward: func(context.Context, net.Listener) error { return sentinel }}
	if err := forward(t.Context(), dir, task, keeper); !errors.Is(err, sentinel) {
		t.Errorf("forward = %v, want the listener's error", err)
	}
}

func TestForwardStopsCleanlyWhenItsCallerCancels(t *testing.T) {
	dir := state.Dir(t.TempDir())
	task := gatewayTask(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	keeper := fakeKeeper{keep: untilCanceled, forward: func(ctx context.Context, listener net.Listener) error {
		close(started)
		return serveUntilCanceled(ctx, listener)
	}}
	stopped := make(chan error, 1)
	go func() { stopped <- forward(ctx, dir, task, keeper) }()
	<-started
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("forward after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop after its caller canceled")
	}
}
