package providertest

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

type Fake struct {
	Facts  providers.Traits
	Rates  providers.Rate
	Handle func(id string, cmd []string, stdin []byte) providers.Result
	Now    func() time.Time

	mu       sync.Mutex
	machines map[string]providers.Machine
	calls    []string
}

var _ providers.Provider = (*Fake)(nil)

func (f *Fake) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) Traits() providers.Traits { return f.Facts }

func (f *Fake) Check(context.Context) error { return nil }

func (f *Fake) Rate(providers.Spec) (providers.Rate, error) { return f.Rates, nil }

func (f *Fake) Create(_ context.Context, spec providers.Spec) (providers.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create %s", spec.Name)
	if spec.Name == "" {
		return providers.Machine{}, fmt.Errorf("fake: a machine needs a name")
	}
	if _, ok := f.machines[spec.Name]; ok {
		return providers.Machine{}, fmt.Errorf("fake %s: %w", spec.Name, providers.ErrExists)
	}
	if f.machines == nil {
		f.machines = map[string]providers.Machine{}
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	machine := providers.Machine{
		ID:        spec.Name,
		Provider:  "fake",
		State:     providers.StateRunning,
		CreatedAt: now().UTC(),
		Labels:    maps.Clone(spec.Labels),
	}
	f.machines[spec.Name] = machine
	return clone(machine), nil
}

func (f *Fake) lookup(id string) (providers.Machine, error) {
	machine, ok := f.machines[id]
	if !ok {
		return providers.Machine{}, fmt.Errorf("fake %s: %w", id, providers.ErrNotFound)
	}
	return machine, nil
}

func (f *Fake) setState(id string, state providers.State) error {
	machine, err := f.lookup(id)
	if err != nil {
		return err
	}
	machine.State = state
	f.machines[id] = machine
	return nil
}

func (f *Fake) Get(_ context.Context, id string) (providers.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	machine, err := f.lookup(id)
	return clone(machine), err
}

func (f *Fake) Wake(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("wake %s", id)
	return f.setState(id, providers.StateRunning)
}

func (f *Fake) Suspend(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("suspend %s", id)
	return f.setState(id, providers.StateSuspended)
}

func (f *Fake) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("destroy %s", id)
	if _, err := f.lookup(id); err != nil {
		return err
	}
	delete(f.machines, id)
	return nil
}

func (f *Fake) Exec(_ context.Context, id string, cmd []string, stdin io.Reader) (providers.Result, error) {
	var input []byte
	if stdin != nil {
		var err error
		if input, err = io.ReadAll(stdin); err != nil {
			return providers.Result{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("exec %s %q", id, cmd)
	if err := f.setState(id, providers.StateRunning); err != nil {
		return providers.Result{}, err
	}
	if f.Handle == nil {
		return providers.Result{}, nil
	}
	return f.Handle(id, slices.Clone(cmd), input), nil
}

func (f *Fake) SSHTarget(_ context.Context, id string) (providers.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.lookup(id); err != nil {
		return providers.Target{}, err
	}
	return providers.Target{
		Host:          id,
		Port:          22,
		User:          "fake",
		IdentityFile:  "/dev/null",
		ProxyCommand:  "false",
		HostKeyPolicy: providers.HostKeyPolicy{Mode: providers.HostKeyProxyTrusted},
	}, nil
}

func (f *Fake) List(_ context.Context, labels map[string]string) ([]providers.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []providers.Machine
	for _, id := range slices.Sorted(maps.Keys(f.machines)) {
		if machine := f.machines[id]; machine.HasLabels(labels) {
			matched = append(matched, clone(machine))
		}
	}
	return matched, nil
}

func clone(machine providers.Machine) providers.Machine {
	machine.Labels = maps.Clone(machine.Labels)
	return machine
}
