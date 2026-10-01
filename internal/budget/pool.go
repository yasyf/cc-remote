package budget

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
	Fingerprint string     `json:"fingerprint"`
	State       SpareState `json:"state"`
	Preparer    int        `json:"preparerPid,omitempty"`
	Request     string     `json:"request,omitempty"`
	ReadyAt     *time.Time `json:"readyAt,omitempty"`
	ClaimedAt   *time.Time `json:"claimedAt,omitempty"`
	ActivatedAt *time.Time `json:"activatedAt,omitempty"`
}

type Spares map[string]*Spare

func (s Store) SparesPath() string {
	return strings.TrimSuffix(s.Path, ".json") + ".spares.json"
}

func (s Store) ReadSpares() (Spares, error) {
	spares := Spares{}
	if _, err := state.Load(s.SparesPath(), &spares); err != nil {
		return nil, err
	}
	return spares, nil
}

func (s Store) UpdateSpares(change func(*Ledger, Spares) error) error {
	unlockSpares, err := state.Lock(s.SparesPath() + ".lock")
	if err != nil {
		return err
	}
	defer unlockSpares()
	unlockLedger, err := state.Lock(s.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlockLedger()
	spares, err := s.ReadSpares()
	if err != nil {
		return err
	}
	ledger, err := s.Read()
	if err != nil {
		return err
	}
	spares.DropRetired(ledger)
	if err := change(ledger, spares); err != nil {
		return err
	}
	if err := state.Save(s.SparesPath(), spares); err != nil {
		return err
	}
	return state.Save(s.Path, ledger)
}

func (sp Spares) DropRetired(l *Ledger) {
	for id := range sp {
		if resource, ok := l.Resources[id]; !ok || resource.Destroyed != nil {
			delete(sp, id)
		}
	}
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

func (sp Spares) Prepare(l *Ledger, id, provider, profile, fingerprint string, rate Rate, reserved float64, preparer int, now time.Time) error {
	if _, ok := l.Resources[id]; ok {
		return fmt.Errorf("the ledger already has %s", id)
	}
	if err := l.Start(id, provider, profile, rate, reserved, now); err != nil {
		return err
	}
	sp[id] = &Spare{Fingerprint: fingerprint, State: Preparing, Preparer: preparer}
	return nil
}

func (sp Spares) MarkReady(l *Ledger, id string, now time.Time) error {
	spare, ok := sp[id]
	if !ok || spare.State != Preparing {
		return fmt.Errorf("%s is not a spare being prepared", id)
	}
	spare.State, spare.ReadyAt = Ready, &now
	return l.Stop(id, now)
}

func (s Store) ClaimedFor(request string) (string, *Resource, error) {
	ledger, err := s.Read()
	if err != nil {
		return "", nil, err
	}
	spares, err := s.ReadSpares()
	if err != nil {
		return "", nil, err
	}
	spares.DropRetired(ledger)
	id, ok := spares.HeldBy(request)
	if !ok {
		return "", nil, nil
	}
	return id, ledger.Resources[id], nil
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
