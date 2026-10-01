package budget

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yasyf/cc-remote/internal/state"
)

const hoursPerMonth = 730

type Rate struct {
	HourlyUSD         float64 `json:"hourlyUSD"`
	StorageGB         float64 `json:"storageGB"`
	StorageGBMonthUSD float64 `json:"storageGBMonthUSD"`
}

func (r Rate) Estimate(hours float64) float64 {
	return hours * (r.HourlyUSD + r.StorageGB*r.StorageGBMonthUSD/hoursPerMonth)
}

type Interval struct {
	Start       time.Time  `json:"start"`
	End         *time.Time `json:"end,omitempty"`
	ReservedUSD float64    `json:"reservedUSD"`
}

type Resource struct {
	Provider  string     `json:"provider"`
	Profile   string     `json:"profile"`
	Rate      Rate       `json:"rate"`
	Created   time.Time  `json:"created"`
	Destroyed *time.Time `json:"destroyed,omitempty"`
	Running   []Interval `json:"running"`
}

func (r *Resource) Spent(now time.Time) float64 {
	var running float64
	for _, interval := range r.Running {
		running += until(interval.End, now).Sub(interval.Start).Hours()
	}
	stored := until(r.Destroyed, now).Sub(r.Created).Hours()
	return running*r.Rate.HourlyUSD + stored*r.Rate.StorageGB*r.Rate.StorageGBMonthUSD/hoursPerMonth
}

func (r *Resource) Committed(now time.Time) float64 {
	committed := r.Spent(now)
	if r.running() {
		current := r.Running[len(r.Running)-1]
		committed += max(0, current.ReservedUSD-now.Sub(current.Start).Hours()*r.Rate.HourlyUSD)
	}
	return committed
}

func (r *Resource) running() bool {
	return len(r.Running) > 0 && r.Running[len(r.Running)-1].End == nil
}

func until(end *time.Time, now time.Time) time.Time {
	if end != nil {
		return *end
	}
	return now
}

type Origin struct {
	Kind     string    `json:"kind"`
	Source   string    `json:"source,omitempty"`
	Imported time.Time `json:"imported"`
}

type Ledger struct {
	Origin    *Origin              `json:"origin,omitempty"`
	Resources map[string]*Resource `json:"resources"`
}

const (
	Created  = "created"
	Imported = "imported"
)

var ErrNoLedger = errors.New("no ledger")

func (l *Ledger) archive(id string, destroyed time.Time) string {
	stamp := id + "~" + destroyed.UTC().Format("20060102T150405.000000000Z")
	key := stamp
	for n := 2; ; n++ {
		if _, taken := l.Resources[key]; !taken {
			return key
		}
		key = stamp + "~" + strconv.Itoa(n)
	}
}

func (l *Ledger) Running(id string) bool {
	resource, ok := l.Resources[id]
	return ok && resource.running()
}

func (l *Ledger) Vacant(id string) error {
	if resource, ok := l.Resources[id]; ok && resource.Destroyed == nil {
		return fmt.Errorf("%s already exists; it is left as it is, so resume it or destroy it before creating again", id)
	}
	return nil
}

func (l *Ledger) Spent(now time.Time) float64 {
	var total float64
	for _, resource := range l.Resources {
		total += resource.Spent(now)
	}
	return total
}

func (l *Ledger) Committed(now time.Time) float64 {
	var total float64
	for _, resource := range l.Resources {
		total += resource.Committed(now)
	}
	return total
}

func (l *Ledger) Start(id, provider, profile string, rate Rate, reserved float64, now time.Time) error {
	resource, ok := l.Resources[id]
	if ok && resource.Destroyed != nil {
		l.Resources[l.archive(id, *resource.Destroyed)] = resource
		ok = false
	}
	if !ok {
		resource = &Resource{Provider: provider, Profile: profile, Rate: rate, Created: now}
		l.Resources[id] = resource
	}
	if !resource.running() {
		resource.Running = append(resource.Running, Interval{Start: now, ReservedUSD: reserved})
	}
	return nil
}

func (l *Ledger) Stop(id string, now time.Time) error {
	resource, ok := l.Resources[id]
	if !ok {
		return fmt.Errorf("the ledger has no %s", id)
	}
	if resource.running() {
		resource.Running[len(resource.Running)-1].End = &now
	}
	return nil
}

func (l *Ledger) Retire(id string, now time.Time) error {
	if err := l.Stop(id, now); err != nil {
		return err
	}
	resource := l.Resources[id]
	if resource.Destroyed == nil {
		resource.Destroyed = &now
	}
	return nil
}

type Guard struct {
	CapUSD     float64
	ReserveUSD float64
}

func (g Guard) Remaining(l *Ledger, now time.Time) float64 {
	return g.CapUSD - g.ReserveUSD - l.Committed(now)
}

type OverBudgetError struct {
	EstimateUSD  float64
	RemainingUSD float64
}

func (e *OverBudgetError) Error() string {
	return fmt.Sprintf("this trial's estimated $%.2f exceeds the $%.2f left under the budget cap after its cleanup reserve", e.EstimateUSD, e.RemainingUSD)
}

func (g Guard) Admit(l *Ledger, estimate float64, now time.Time) error {
	remaining := g.Remaining(l, now)
	if !finite(remaining) || !finite(estimate) || estimate < 0 {
		return fmt.Errorf("the budget cannot be judged: $%v remains against an estimate of $%v", remaining, estimate)
	}
	if estimate > remaining {
		return &OverBudgetError{EstimateUSD: estimate, RemainingUSD: remaining}
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type Store struct {
	Path string
}

func (s Store) Read() (*Ledger, error) {
	ledger := &Ledger{Resources: map[string]*Resource{}}
	found, err := state.Load(s.Path, ledger)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w at %s: run `cc-remote ledger init` to start one or `cc-remote ledger import <path>` to carry an earlier one over", ErrNoLedger, s.Path)
	}
	if ledger.Resources == nil {
		ledger.Resources = map[string]*Resource{}
	}
	return ledger, nil
}

func (s Store) Init(now time.Time) error {
	return s.create(&Ledger{Origin: &Origin{Kind: Created, Imported: now}, Resources: map[string]*Resource{}})
}

func (s Store) Import(source string, now time.Time) (*Ledger, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	ledger, err := Store{Path: absolute}.Read()
	if err != nil {
		return nil, err
	}
	ledger.Origin = &Origin{Kind: Imported, Source: absolute, Imported: now}
	return ledger, s.create(ledger)
}

func (s Store) create(ledger *Ledger) error {
	unlock, err := state.Lock(s.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	_, err = os.Stat(s.Path)
	switch {
	case err == nil:
		return fmt.Errorf("%s already exists; cc-remote never replaces a ledger", s.Path)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return state.Save(s.Path, ledger)
}

func (s Store) Update(change func(*Ledger) error) error {
	unlock, err := state.Lock(s.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	ledger, err := s.Read()
	if err != nil {
		return err
	}
	if err := change(ledger); err != nil {
		return err
	}
	return state.Save(s.Path, ledger)
}
