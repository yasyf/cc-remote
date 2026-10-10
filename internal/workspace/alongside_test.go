package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

func TestEveryCreateRunsItsAlongsideStepOnceBeforeItPublishes(t *testing.T) {
	for _, spare := range []bool{false, true} {
		t.Run(fmt.Sprintf("spare=%v", spare), func(t *testing.T) {
			h := newHarness(t, false)
			var machines []string
			published := -1
			h.session.Alongside = func(_ context.Context, machine string) error {
				machines = append(machines, machine)
				published = h.machine.ran("ws-1", publishes)
				return nil
			}
			create := h.session.Create
			if spare {
				create = h.session.CreateSpare
			}
			if _, err := create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(machines, []string{"ws-1"}) || published != 0 || h.machine.ran("ws-1", publishes) != 1 {
				t.Errorf("the alongside step ran on %q after %d publishes, and the create published %d times; want one run on ws-1 before the one publish", machines, published, h.machine.ran("ws-1", publishes))
			}
		})
	}
}

func TestAFailedAlongsideStepFailsTheCreateAndDiscardsWhatItMade(t *testing.T) {
	h := newHarness(t, false)
	refused := errors.New("the alongside step refused")
	h.session.Alongside = func(context.Context, string) error { return refused }
	result, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
	if result != nil || !errors.Is(err, refused) {
		t.Fatalf("Create = %+v, %v; want the alongside step's refusal", result, err)
	}
	if ran := h.machine.ran("ws-1", publishes); ran != 0 {
		t.Errorf("the failed create published %d times", ran)
	}
	if _, err := h.fake.Get(context.Background(), "ws-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("the machine survived the failed create: %v", err)
	}
	if _, found := h.record("ws-1"); found {
		t.Error("the failed create left a record")
	}
}

func TestResumeAndReuseNeverRunTheAlongsideStep(t *testing.T) {
	ctx := context.Background()
	for _, spare := range []bool{false, true} {
		t.Run(fmt.Sprintf("spare=%v", spare), func(t *testing.T) {
			h := newHarness(t, false)
			create := h.session.Create
			if spare {
				create = h.session.CreateSpare
			}
			if _, err := create(ctx, "ws-1", Source{Ref: "main"}); err != nil {
				t.Fatal(err)
			}
			if err := h.session.Suspend(ctx, "ws-1"); err != nil {
				t.Fatal(err)
			}
			runs := 0
			h.session.Alongside = func(context.Context, string) error {
				runs++
				return nil
			}
			var err error
			if spare {
				_, err = h.session.Reuse(ctx, "ws-1", Source{Ref: "feature"})
			} else {
				_, err = h.session.Resume(ctx, "ws-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			if runs != 0 {
				t.Errorf("the alongside step ran %d times on a workspace that was not being created", runs)
			}
		})
	}
}
