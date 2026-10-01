package tailnet

import (
	"fmt"
	"path/filepath"

	"github.com/yasyf/cc-remote/internal/state"
)

type Binding struct {
	NodeID     string `json:"nodeId,omitempty"`
	Unresolved bool   `json:"unresolved,omitempty"`
}

func (b Binding) String() string {
	switch {
	case b.Unresolved:
		return "unresolved"
	case b.NodeID == "":
		return "none"
	}
	return b.NodeID
}

type Bindings struct {
	Dir string
}

type BindingChangedError struct {
	Resource string
	Expected Binding
	Found    Binding
}

func (e *BindingChangedError) Error() string {
	return fmt.Sprintf("workspace %s is bound to %s, not %s; another run changed it", e.Resource, e.Found, e.Expected)
}

func (b Bindings) path(resource string) string {
	return filepath.Join(b.Dir, resource+".json")
}

func (b Bindings) Read(resource string) (Binding, error) {
	var binding Binding
	if _, err := state.Load(b.path(resource), &binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

type Bound struct {
	bindings Bindings
	held     *state.Held
}

func (b Bindings) Of(held *state.Held) *Bound {
	return &Bound{bindings: b, held: held}
}

func (b *Bound) Resource() string {
	return b.held.Name
}

func (b *Bound) Read() (Binding, error) {
	return b.bindings.Read(b.held.Name)
}

func (b *Bound) Transition(from, to Binding) error {
	found, err := b.Read()
	if err != nil {
		return err
	}
	if found != from {
		return &BindingChangedError{Resource: b.held.Name, Expected: from, Found: found}
	}
	path := b.bindings.path(b.held.Name)
	if to == (Binding{}) {
		return state.Remove(path)
	}
	return state.Save(path, to)
}
