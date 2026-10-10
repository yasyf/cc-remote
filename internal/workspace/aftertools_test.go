package workspace

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

func releasedBeside(t *testing.T, parked *hold) func(context.Context, string) {
	t.Helper()
	var late atomic.Bool
	released := make(chan struct{})
	go func() {
		select {
		case <-parked.entered:
		case <-t.Context().Done():
			return
		}
		select {
		case <-released:
		case <-t.Context().Done():
		case <-time.After(5 * time.Second):
			late.Store(true)
			parked.release <- providers.Result{}
		}
	}()
	return func(ctx context.Context, _ string) {
		select {
		case <-parked.entered:
		case <-ctx.Done():
			t.Errorf("the after-tools step was cancelled before %q ran: %v", parked.fragment, context.Cause(ctx))
			return
		}
		if late.Load() {
			t.Errorf("%q finished before the after-tools step ran", parked.fragment)
			return
		}
		close(released)
		parked.release <- providers.Result{}
	}
}

func TestCreateRunsItsAfterToolsStepOnceTheToolsAreInstalledAndBeforeItPublishes(t *testing.T) {
	h := newHarness(t, false)
	var machines []string
	installed, published := -1, -1
	h.session.AfterTools = func(_ context.Context, machine string) {
		machines = append(machines, machine)
		installed, published = h.machine.ran("ws-1", installs), h.machine.ran("ws-1", publishes)
	}
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(machines, []string{"ws-1"}) || installed != 1 || published != 0 || h.machine.ran("ws-1", publishes) != 1 {
		t.Errorf("the after-tools step ran on %q after %d installs and %d publishes, and the create published %d times; want one run on ws-1 between the install and the one publish", machines, installed, published, h.machine.ran("ws-1", publishes))
	}
}

func TestCreateConfiguresWhileItsAfterToolsStepRuns(t *testing.T) {
	h := newHarness(t, false)
	h.session.AfterTools = releasedBeside(t, h.machine.hold(t, configures))
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedCreateCancelsItsAfterToolsStepAndWaitsForIt(t *testing.T) {
	h := newHarness(t, false)
	refused := errors.New("the configure step refused")
	configured := h.machine.hold(t, configures)
	returned := false
	h.session.AfterTools = func(ctx context.Context, _ string) {
		<-configured.entered
		configured.release <- providers.Result{Stderr: []byte(refused.Error()), ExitCode: 1}
		<-ctx.Done()
		returned = true
	}
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if result != nil || err == nil || !returned {
		t.Fatalf("Create = %+v, %v with the after-tools step returned %t; want the configure failure after the step was cancelled", result, err, returned)
	}
}

func TestReuseRefreshesTheCheckoutWhileItsAfterToolsStepRuns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	runs := 0
	h.session.AfterTools = func(context.Context, string) { runs++ }
	if _, err := h.session.CreateSpare(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("the after-tools step ran %d times while the spare was created", runs)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	checked := -1
	beside := releasedBeside(t, h.machine.hold(t, configures))
	h.session.AfterTools = func(ctx context.Context, machine string) {
		runs++
		checked = h.machine.ran("ws-1", readies)
		beside(ctx, machine)
	}
	if _, err := h.session.Reuse(ctx, "ws-1", Source{Ref: "feature"}); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || checked != 1 {
		t.Errorf("the after-tools step ran %d times after %d stamp checks; want one run after the one check", runs, checked)
	}
}

func TestAFailedReuseCancelsItsAfterToolsStepAndWaitsForIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	if _, err := h.session.CreateSpare(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	configured := h.machine.hold(t, configures)
	returned := false
	h.session.AfterTools = func(ctx context.Context, _ string) {
		<-configured.entered
		configured.release <- providers.Result{Stderr: []byte("the configure step refused"), ExitCode: 1}
		<-ctx.Done()
		returned = true
	}
	result, err := h.session.Reuse(ctx, "ws-1", Source{Ref: "feature"})
	if result != nil || err == nil || !returned {
		t.Fatalf("Reuse = %+v, %v with the after-tools step returned %t; want the configure failure after the step was cancelled", result, err, returned)
	}
	if _, found := h.record("ws-1"); !found {
		t.Error("the failed reuse dropped its record")
	}
}

func TestResumeNeverRunsTheAfterToolsStep(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, false)
	if _, err := h.session.Create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.session.Suspend(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	runs := 0
	h.session.AfterTools = func(context.Context, string) { runs++ }
	if _, err := h.session.Resume(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("the after-tools step ran %d times on a resumed workspace", runs)
	}
}
