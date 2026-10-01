package spare

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/state"
)

type SpareState string

const (
	Preparing SpareState = "preparing"
	Ready     SpareState = "ready"
	Claimed   SpareState = "claimed"
	Draining  SpareState = "draining"
)

type Spare struct {
	Provider    string     `json:"provider"`
	Profile     string     `json:"profile"`
	Fingerprint string     `json:"fingerprint"`
	State       SpareState `json:"state"`
	Preparer    int        `json:"preparerPid,omitempty"`
	Request     string     `json:"request,omitempty"`
	ReadyAt     *time.Time `json:"readyAt,omitempty"`
	ClaimedAt   *time.Time `json:"claimedAt,omitempty"`
	ActivatedAt *time.Time `json:"activatedAt,omitempty"`
}

type Spares map[string]*Spare

type Store struct{ Path string }

func (s Store) Read() (Spares, error) {
	spares := Spares{}
	if _, err := state.Load(s.Path, &spares); err != nil {
		return nil, err
	}
	return spares, nil
}

func (s Store) Update(change func(Spares) error) error {
	unlock, err := state.Lock(s.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	spares, err := s.Read()
	if err != nil {
		return err
	}
	if err := change(spares); err != nil {
		return err
	}
	return state.Save(s.Path, spares)
}

func (sp Spares) Pooled(fingerprint string) int {
	var count int
	for _, spare := range sp {
		if spare.Fingerprint == fingerprint && (spare.State == Preparing || spare.State == Ready) {
			count++
		}
	}
	return count
}

func (sp Spares) Prepare(id, provider, profile, fingerprint string, preparer int) error {
	if _, ok := sp[id]; ok {
		return fmt.Errorf("%s is already recorded as a spare", id)
	}
	sp[id] = &Spare{Provider: provider, Profile: profile, Fingerprint: fingerprint, State: Preparing, Preparer: preparer}
	return nil
}

func (sp Spares) MarkReady(id string, now time.Time) error {
	spare, ok := sp[id]
	if !ok || spare.State != Preparing {
		return fmt.Errorf("%s is not a spare being prepared", id)
	}
	spare.State, spare.ReadyAt = Ready, &now
	return nil
}

func (s Store) ClaimedFor(request string) (string, *Spare, error) {
	spares, err := s.Read()
	if err != nil {
		return "", nil, err
	}
	id, ok := spares.HeldBy(request)
	if !ok {
		return "", nil, nil
	}
	return id, spares[id], nil
}

func (sp Spares) HeldBy(request string) (string, bool) {
	for id, spare := range sp {
		if spare.State == Claimed && spare.Request == request {
			return id, true
		}
	}
	return "", false
}

func (sp Spares) Activate(id string, now time.Time) error {
	spare, ok := sp[id]
	if !ok || spare.State != Claimed {
		return fmt.Errorf("%s is not a claimed spare", id)
	}
	spare.ActivatedAt = &now
	return nil
}

func (sp Spares) Claim(fingerprint, request string, now time.Time) (string, bool) {
	if id, ok := sp.HeldBy(request); ok {
		return id, true
	}
	var ready []string
	for id, spare := range sp {
		if spare.State == Ready && spare.Fingerprint == fingerprint {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		return request, false
	}
	slices.SortFunc(ready, func(a, b string) int {
		if c := sp[a].ReadyAt.Compare(*sp[b].ReadyAt); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	spare := sp[ready[0]]
	spare.State, spare.Request, spare.ClaimedAt = Claimed, request, &now
	return ready[0], true
}

func (sp Spares) Drain(unwanted func(id string, spare *Spare) bool) []string {
	var drained []string
	for id, spare := range sp {
		if spare.State != Claimed && unwanted(id, spare) {
			spare.State = Draining
			drained = append(drained, id)
		}
	}
	slices.Sort(drained)
	return drained
}
