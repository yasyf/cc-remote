package tailnet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-remote/internal/state"
)

func TestBindingsMoveOnlyFromTheStateTheHolderSaw(t *testing.T) {
	dir := state.Dir(t.TempDir())
	bindings := Bindings{Dir: dir.Tailnet()}
	none, intent, mine, other := Binding{}, Binding{Unresolved: true}, Binding{NodeID: "nMINE"}, Binding{NodeID: "nOTHER"}
	if got, err := bindings.Read("ws-1"); err != nil || got != none {
		t.Fatalf("an unbound resource reads %+v, %v", got, err)
	}
	held, err := dir.Hold("ws-1")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	bound := bindings.Of(held)
	for _, step := range []struct{ from, to Binding }{{none, intent}, {intent, mine}} {
		if err := bound.Transition(step.from, step.to); err != nil {
			t.Fatal(err)
		}
		if got, err := bindings.Read("ws-1"); err != nil || got != step.to {
			t.Errorf("after %v -> %v read %v, %v", step.from, step.to, got, err)
		}
	}
	var changed *BindingChangedError
	for _, stale := range []struct{ from, to Binding }{{none, other}, {intent, other}, {other, none}, {none, none}} {
		err := bound.Transition(stale.from, stale.to)
		if !errors.As(err, &changed) || changed.Found != mine {
			t.Errorf("a stale %v -> %v transition passed: %v", stale.from, stale.to, err)
		}
	}
	if got, _ := bound.Read(); got != mine {
		t.Errorf("a refused transition rewrote the binding to %v", got)
	}
	if err := bound.Transition(mine, none); err != nil {
		t.Fatal(err)
	}
	if got, err := bound.Read(); err != nil || got != none {
		t.Errorf("clearing left %v, %v", got, err)
	}
	if err := bound.Transition(none, none); err != nil {
		t.Errorf("clearing an unbound resource: %v", err)
	}
	entries, _ := os.ReadDir(bindings.Dir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("a temporary file survived: %s", entry.Name())
		}
	}
}

func TestBindingStringsNameTheThreeStates(t *testing.T) {
	if (Binding{}).String() != "none" || (Binding{Unresolved: true}).String() != "unresolved" || (Binding{NodeID: "n1"}).String() != "n1" {
		t.Error("Binding.String")
	}
}
