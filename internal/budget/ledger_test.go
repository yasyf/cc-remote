package budget

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.6f, want %.6f", what, got, want)
	}
}

func TestEstimateCoversComputeAndStorage(t *testing.T) {
	rate := Rate{HourlyUSD: 0.96, StorageGB: 125, StorageGBMonthUSD: 0.2}
	near(t, "estimate", rate.Estimate(3), 3*(0.96+125*0.2/730))
}

func TestSpentCountsRunningAndStoredTime(t *testing.T) {
	ledger := &Ledger{Resources: map[string]*Resource{}}
	rate := Rate{HourlyUSD: 1, StorageGB: 730, StorageGBMonthUSD: 1}
	if err := ledger.Start("a", "namespace", "agents", rate, 0, epoch); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Stop("a", epoch.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	near(t, "suspended spend", ledger.Spent(epoch.Add(4*time.Hour)), 2+4)
	if err := ledger.Retire("a", epoch.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	near(t, "retired spend", ledger.Spent(epoch.Add(9*time.Hour)), 2+5)
}

func TestGuardRefusesATrialOverTheRemainder(t *testing.T) {
	guard := Guard{CapUSD: 100, ReserveUSD: 10}
	ledger := &Ledger{Resources: map[string]*Resource{}}
	rate := Rate{HourlyUSD: 10}
	if err := ledger.Start("a", "sprites", "agents", rate, 0, epoch); err != nil {
		t.Fatal(err)
	}
	now := epoch.Add(8 * time.Hour)
	if err := guard.Admit(ledger, 10, now); err != nil {
		t.Errorf("a trial inside the remaining $10 was refused: %v", err)
	}
	err := guard.Admit(ledger, 10.01, now)
	var over *OverBudgetError
	if !errors.As(err, &over) {
		t.Fatalf("err = %v", err)
	}
	near(t, "remaining", over.RemainingUSD, 10)
}

func TestReservationsCountBeforeSpendAccrues(t *testing.T) {
	guard := Guard{CapUSD: 100, ReserveUSD: 10}
	ledger := &Ledger{Resources: map[string]*Resource{}}
	rate := Rate{HourlyUSD: 1}
	for _, id := range []string{"a", "b", "c"} {
		if err := guard.Admit(ledger, 30, epoch); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if err := ledger.Start(id, "namespace", "agents", rate, 30, epoch); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.Admit(ledger, 30, epoch); err == nil {
		t.Fatal("a fourth $30 trial fit under $90 already reserved")
	}
	near(t, "committed after an hour", ledger.Committed(epoch.Add(time.Hour)), 90)
	near(t, "committed past the reservation", ledger.Committed(epoch.Add(40*time.Hour)), 120)
}

func TestSuspendReleasesTheReservation(t *testing.T) {
	ledger := &Ledger{Resources: map[string]*Resource{}}
	if err := ledger.Start("a", "namespace", "agents", Rate{HourlyUSD: 1}, 30, epoch); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Stop("a", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	near(t, "committed", ledger.Committed(epoch.Add(2*time.Hour)), 1)
}

func TestRetiredResourcesStayRetired(t *testing.T) {
	ledger := &Ledger{Resources: map[string]*Resource{}}
	if err := ledger.Start("a", "namespace", "agents", Rate{}, 0, epoch); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Retire("a", epoch); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Start("a", "namespace", "agents", Rate{HourlyUSD: 2}, 0, epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	history := ledger.Resources["a~20260930T120000Z"]
	if history == nil || history.Destroyed == nil || ledger.Resources["a"].Rate.HourlyUSD != 2 || ledger.Resources["a"].Destroyed != nil {
		t.Fatalf("a recreated name did not archive its history: %+v", ledger.Resources)
	}
	if err := ledger.Retire("a", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ledger.Resources["a"].Destroyed == nil || ledger.Resources["a~20260930T120000Z"] != history {
		t.Fatal("retiring the recreated resource touched its history")
	}
	if err := ledger.Start("a", "namespace", "agents", Rate{}, 0, epoch.Add(time.Hour)); err != nil || ledger.Resources["a~20260930T130000Z"] == nil {
		t.Fatalf("a second recreation did not archive the second life: %v", err)
	}
}

func TestAStoreRefusesWorkUntilALedgerIsStartedOrImported(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "ledger.json")}
	if _, err := store.Read(); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("read of a missing ledger = %v", err)
	}
	if err := store.Update(func(*Ledger) error { return nil }); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("update of a missing ledger = %v", err)
	}
	if err := store.Init(epoch); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(epoch); err == nil {
		t.Fatal("init replaced a ledger")
	}
	ledger, err := store.Read()
	if err != nil || ledger.Origin == nil || ledger.Origin.Kind != Created || !ledger.Origin.Imported.Equal(epoch) {
		t.Fatalf("ledger = %+v, %v", ledger, err)
	}
}

func TestImportCarriesAnEarlierLedgerOverWithProvenance(t *testing.T) {
	source := Store{Path: filepath.Join(t.TempDir(), "forge-pilot.json")}
	if err := source.Init(epoch); err != nil {
		t.Fatal(err)
	}
	if err := source.Update(func(ledger *Ledger) error {
		return ledger.Start("kept", "namespace", "agents", Rate{HourlyUSD: 1}, 5, epoch)
	}); err != nil {
		t.Fatal(err)
	}
	store := Store{Path: filepath.Join(t.TempDir(), "default.json")}
	imported, err := store.Import(source.Path, epoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Origin == nil || ledger.Origin.Kind != Imported || ledger.Origin.Source != source.Path || !ledger.Running("kept") || imported.Origin.Kind != Imported {
		t.Fatalf("imported ledger = %+v", ledger)
	}
	if _, err := store.Import(source.Path, epoch); err == nil {
		t.Fatal("import replaced a ledger")
	}
	if _, err := (Store{Path: filepath.Join(t.TempDir(), "other.json")}).Import(filepath.Join(t.TempDir(), "missing.json"), epoch); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("import of a missing source = %v", err)
	}
}

func TestAdmitRefusesABudgetItCannotJudge(t *testing.T) {
	ledger := &Ledger{Resources: map[string]*Resource{}}
	for _, guard := range []Guard{{CapUSD: math.NaN()}, {CapUSD: math.Inf(1)}, {CapUSD: 100, ReserveUSD: math.NaN()}} {
		if err := guard.Admit(ledger, 1, epoch); err == nil {
			t.Errorf("%+v admitted a trial", guard)
		}
	}
	for _, estimate := range []float64{math.NaN(), math.Inf(1), -1} {
		if err := (Guard{CapUSD: 100}).Admit(ledger, estimate, epoch); err == nil {
			t.Errorf("an estimate of %v was admitted", estimate)
		}
	}
	if err := (Guard{CapUSD: 100}).Admit(ledger, 1, epoch); err != nil {
		t.Error(err)
	}
}

func TestStoreSerializesConcurrentCreates(t *testing.T) {
	store := initialized(t)
	guard := Guard{CapUSD: 100, ReserveUSD: 10}
	var wg sync.WaitGroup
	admitted := make(chan string, 8)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		wg.Go(func() {
			err := store.Update(func(ledger *Ledger) error {
				if err := guard.Admit(ledger, 20, epoch); err != nil {
					return err
				}
				return ledger.Start(id, "sprites", "agents", Rate{HourlyUSD: 1}, 20, epoch)
			})
			if err == nil {
				admitted <- id
			}
		})
	}
	wg.Wait()
	close(admitted)
	if len(admitted) != 4 {
		t.Errorf("%d $20 trials fit under a $90 remainder", len(admitted))
	}
	ledger, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Resources) != 4 {
		t.Errorf("the ledger holds %d resources", len(ledger.Resources))
	}
}
