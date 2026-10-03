package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

func computeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false)
	h.fake.Compute = &providers.ComputeInstance{Container: "agent", Endpoint: "https://compute.test", ContainerPort: 18766, ExportedPort: 30000, IngressDomain: "us.test.nscluster.cloud"}
	return h
}

func TestRetainOwnsTheAllocatedMachineBeforeItIsReady(t *testing.T) {
	h := computeHarness(t)
	start := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	deadline := start.Add(3 * time.Minute)
	h.fake.Compute.Deadline = deadline
	var now atomic.Int64
	now.Store(start.UnixNano())
	var retainedAt atomic.Int64
	h.fake.Initialize = func(machine providers.Machine) error {
		if retainedAt.Load() == 0 {
			t.Errorf("%s became ready before its keeper was retained", machine.ID)
		}
		now.Add(int64(4 * time.Minute))
		return nil
	}
	handle := h.fake.Handle
	h.fake.Handle = func(id string, cmd []string, stdin []byte) providers.Result {
		if strings.Contains(strings.Join(cmd, " "), prepares) && retainedAt.Load() == 0 {
			t.Error("the profile preparation ran before the keeper was retained")
		}
		return handle(id, cmd, stdin)
	}
	calls := 0
	h.session.Retain = func(_ context.Context, record *Record) error {
		calls++
		saved, found := h.record("ws-1")
		if !found || !saved.Unverified || saved.Machine != "ws-1" || saved.Compute == nil || saved.Compute.InstanceID != "ws-1" || !saved.Compute.Deadline.Equal(deadline) {
			t.Errorf("record on disk at retain = %+v, %v; want the exact allocated instance and its short deadline durably recorded before the create is confirmed", saved, found)
		}
		if record.Machine != "ws-1" || record.Compute == nil {
			t.Errorf("retained record = %+v", record)
		}
		if len(h.machine.scripts["ws-1"]) != 0 {
			t.Error("the machine ran scripts before its keeper was retained")
		}
		retainedAt.Store(now.Load())
		return nil
	}
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Machine != "ws-1" {
		t.Errorf("retain ran %d times for %+v, want once", calls, result)
	}
	if record, found := h.record("ws-1"); !found || record.Unverified {
		t.Errorf("record after the create = %+v, %v; want it verified", record, found)
	}
	if at := time.Unix(0, retainedAt.Load()); !at.Before(deadline) || !time.Unix(0, now.Load()).After(deadline) {
		t.Errorf("retained at %v and ready at %v; want the keeper active before the %v deadline the readiness outlasts", at, time.Unix(0, now.Load()), deadline)
	}
}

func TestRetainRunsBeforeResumeWakesTheInstance(t *testing.T) {
	h := computeHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	before := len(h.fake.Calls())
	calls := 0
	h.session.Retain = func(_ context.Context, record *Record) error {
		calls++
		machine, err := h.fake.Get(ctx, "ws-1")
		if err != nil || machine.State != providers.StateSuspended || strings.Contains(strings.Join(h.fake.Calls()[before:], "\n"), "wake") {
			t.Errorf("the keeper was retained after the wake: %+v, %v", machine, err)
		}
		if record.Machine != "ws-1" {
			t.Errorf("retained record = %+v", record)
		}
		return nil
	}
	if _, err := h.session.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("retain ran %d times on resume, want once", calls)
	}
}

func TestAFailedRetainKeepsTheAllocatedMachineRecorded(t *testing.T) {
	h := computeHarness(t)
	ctx := context.Background()
	refused := errors.New("the gateway forward could not hold its listener")
	h.session.Retain = func(context.Context, *Record) error { return refused }
	_, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"})
	if !errors.Is(err, refused) || !errors.Is(err, providers.ErrAmbiguous) || !strings.Contains(err.Error(), "ws-1 was allocated as ws-1 but not provisioned, so its record is kept: run destroy ws-1") {
		t.Fatalf("Create = %v, want the retain failure on the known machine", err)
	}
	if strings.Contains(h.calls(), "destroy") || len(h.machine.scripts["ws-1"]) != 0 {
		t.Errorf("calls = %s, scripts %q; want the known machine neither destroyed nor provisioned", h.calls(), h.machine.scripts["ws-1"])
	}
	record, found := h.record("ws-1")
	if !found || !record.Unverified || record.Machine != "ws-1" || record.Compute == nil || record.Compute.InstanceID != "ws-1" {
		t.Fatalf("record = %+v, %v; want the exact known machine kept unverified", record, found)
	}
	if err := h.session.Destroy(ctx, "ws-1"); err != nil {
		t.Fatalf("Destroy of the known machine = %v", err)
	}
	if !strings.Contains(h.calls(), "destroy ws-1") {
		t.Errorf("calls = %s; want the explicit destroy to reach the known machine", h.calls())
	}
}

func TestAnInitializationFailureKeepsTheRetainedMachineRecorded(t *testing.T) {
	h := computeHarness(t)
	stalled := errors.New("the new workspace volume was not initialized within readyTimeout 10m0s")
	h.fake.Initialize = func(providers.Machine) error { return stalled }
	calls := 0
	h.session.Retain = func(context.Context, *Record) error {
		calls++
		return nil
	}
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); !errors.Is(err, stalled) || !errors.Is(err, providers.ErrAmbiguous) || !strings.Contains(err.Error(), "run destroy ws-1") {
		t.Fatalf("Create = %v, want the initialization failure on the known machine", err)
	}
	if calls != 1 {
		t.Errorf("retain ran %d times, want the one keeper retained before the failure", calls)
	}
	if strings.Contains(h.calls(), "destroy") || len(h.machine.scripts["ws-1"]) != 0 {
		t.Errorf("calls = %s, scripts %q; want the known machine neither destroyed nor provisioned", h.calls(), h.machine.scripts["ws-1"])
	}
	if record, found := h.record("ws-1"); !found || !record.Unverified || record.Machine != "ws-1" {
		t.Errorf("record = %+v, %v; want the known machine kept unverified", record, found)
	}
}

func TestABareCreateRetainsNothingAndDeliversComputeAccess(t *testing.T) {
	h := computeHarness(t)
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SSH != nil || result.Compute == nil || result.Compute.InstanceID != "ws-1" || result.Compute.ContainerPort != 18766 {
		t.Errorf("result = %+v, compute %+v; want Compute access and no SSH", result, result.Compute)
	}
	if _, err := os.Stat(h.session.State.SSH("ws-1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Compute workspace got an ssh fragment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(string(h.session.State), "orca")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a bare create left Orca keeper state: %v", err)
	}
	if record, found := h.record("ws-1"); !found || record.Compute == nil || record.Compute.Endpoint != "https://compute.test" {
		t.Errorf("record = %+v, %v", record, found)
	}
}

type extending struct {
	providers.Provider
	granted time.Time
	asked   []time.Duration
}

func (e *extending) Extend(_ context.Context, _ string, by time.Duration) (time.Time, error) {
	e.asked = append(e.asked, by)
	return e.granted, nil
}

func TestExtendRecordsTheDeadlineTheProviderAnswered(t *testing.T) {
	h := computeHarness(t)
	ctx := context.Background()
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.session.Extend(ctx, "ws-1", time.Hour); err == nil || err.Error() != "provider fake has no explicit lifetime to extend" {
		t.Errorf("Extend on a provider without lifetimes = %v", err)
	}
	granted := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	provider := &extending{Provider: h.provider, granted: granted}
	h.session.Provider = provider
	record, err := h.session.Extend(ctx, "ws-1", 2*time.Hour)
	if err != nil || record.Compute == nil || !record.Compute.Deadline.Equal(granted) || len(provider.asked) != 1 || provider.asked[0] != 2*time.Hour {
		t.Fatalf("Extend = %+v, %v after %v", record, err, provider.asked)
	}
	if saved, found := h.record("ws-1"); !found || !saved.Compute.Deadline.Equal(granted) {
		t.Errorf("saved record = %+v, %v", saved, found)
	}
	plain := newHarness(t, false)
	if _, err := plain.session.Create(ctx, "ws-2", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	plain.session.Provider = &extending{Provider: plain.provider, granted: granted}
	if _, err := plain.session.Extend(ctx, "ws-2", time.Hour); err == nil || err.Error() != "ws-2 has no provisioned instance with a recorded deadline to extend" {
		t.Errorf("Extend of an SSH workspace = %v", err)
	}
}
