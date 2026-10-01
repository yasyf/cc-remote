package budget

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func pooled(t *testing.T, fingerprint string, ready ...string) (*Ledger, Spares) {
	t.Helper()
	ledger, spares := &Ledger{Resources: map[string]*Resource{}}, Spares{}
	for i, id := range ready {
		if err := spares.Prepare(ledger, id, "sprites", "agents", fingerprint, Rate{HourlyUSD: 1}, 3, 1, epoch); err != nil {
			t.Fatal(err)
		}
		if err := spares.MarkReady(ledger, id, epoch.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return ledger, spares
}

func TestClaimTakesTheLongestReadySpare(t *testing.T) {
	_, spares := pooled(t, "f", "b", "a")
	name, claimed := spares.Claim("f", "ow-req", epoch.Add(time.Hour))
	if !claimed || name != "b" {
		t.Fatalf("claimed %q, %v; want b, the first ready", name, claimed)
	}
	if spare := spares["b"]; spare.State != Claimed || spare.Request != "ow-req" || !spare.ClaimedAt.Equal(epoch.Add(time.Hour)) {
		t.Errorf("claimed spare = %+v", spare)
	}
	if spares.Pooled("f") != 1 {
		t.Errorf("pooled = %d after one of two was claimed", spares.Pooled("f"))
	}
}

func TestARetriedRequestKeepsItsSpare(t *testing.T) {
	_, spares := pooled(t, "f", "a", "b")
	first, _ := spares.Claim("f", "ow-req", epoch)
	second, claimed := spares.Claim("f", "ow-req", epoch.Add(time.Minute))
	if !claimed || second != first {
		t.Fatalf("the retry got %q, %v; the first attempt claimed %q", second, claimed, first)
	}
	if spares.Pooled("f") != 1 {
		t.Errorf("the retry claimed a second spare; pooled = %d", spares.Pooled("f"))
	}
}

func TestAClaimedSpareIsNeverReassigned(t *testing.T) {
	ledger, spares := pooled(t, "f", "a")
	if name, _ := spares.Claim("f", "ow-one", epoch); name != "a" {
		t.Fatalf("claimed %q", name)
	}
	if name, claimed := spares.Claim("f", "ow-two", epoch); claimed || name != "ow-two" {
		t.Errorf("a second request got %q, %v", name, claimed)
	}
	if drained := spares.Drain(func(string, *Spare) bool { return true }); len(drained) != 0 {
		t.Errorf("drain took claimed spares %v", drained)
	}
	if err := ledger.Retire("a", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spares.DropRetired(ledger)
	if _, ok := spares["a"]; ok {
		t.Error("a destroyed spare stayed in the pool")
	}
	if name, claimed := spares.Claim("f", "ow-one", epoch.Add(2*time.Hour)); claimed {
		t.Errorf("the destroyed spare %q was claimed again", name)
	}
}

func TestClaimFallsThroughWithoutAMatchingReadySpare(t *testing.T) {
	ledger, spares := pooled(t, "old", "a")
	if err := spares.Prepare(ledger, "b", "sprites", "agents", "new", Rate{}, 0, 1, epoch); err != nil {
		t.Fatal(err)
	}
	name, claimed := spares.Claim("new", "ow-req", epoch)
	if claimed || name != "ow-req" {
		t.Errorf("claimed %q, %v from a stale ready spare and one still preparing", name, claimed)
	}
}

func TestAWorkspaceIsVacantOnlyOnceDestroyed(t *testing.T) {
	ledger, _ := pooled(t, "f")
	if err := ledger.Vacant("ow-req"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Start("ow-req", "sprites", "agents", Rate{}, 0, epoch); err != nil {
		t.Fatal(err)
	}
	if ledger.Vacant("ow-req") == nil {
		t.Error("a running workspace is vacant")
	}
	if err := ledger.Stop("ow-req", epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ledger.Vacant("ow-req") == nil {
		t.Error("a suspended workspace is vacant")
	}
	if err := ledger.Retire("ow-req", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Vacant("ow-req"); err != nil {
		t.Errorf("a destroyed workspace is not vacant: %v", err)
	}
}

func TestAReadySpareStopsRunning(t *testing.T) {
	ledger, spares := pooled(t, "f", "a")
	near(t, "committed once ready", ledger.Committed(epoch.Add(10*time.Hour)), 0)
	if err := spares.MarkReady(ledger, "a", epoch); err == nil {
		t.Error("a ready spare was marked ready again")
	}
}

func TestDrainTakesOnlyUnclaimedSparesItIsAskedFor(t *testing.T) {
	_, spares := pooled(t, "f", "a", "b", "c")
	spares.Claim("f", "ow-req", epoch)
	drained := spares.Drain(func(id string, _ *Spare) bool { return id != "c" })
	if !slices.Equal(drained, []string{"b"}) {
		t.Fatalf("drained %v, want [b]", drained)
	}
	if spares["b"].State != Draining || spares.Pooled("f") != 1 {
		t.Errorf("after the drain b is %s and %d are pooled", spares["b"].State, spares.Pooled("f"))
	}
	if name, _ := spares.Claim("f", "ow-other", epoch); name != "c" {
		t.Errorf("a claim after the drain got %q", name)
	}
}

func TestConcurrentClaimsNeverShareASpare(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "ledger.json")}
	if err := store.UpdateSpares(func(ledger *Ledger, spares Spares) error {
		seeded, seededSpares := pooled(t, "f", "a", "b", "c")
		*ledger = *seeded
		maps.Copy(spares, seededSpares)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make(chan [2]string, 8)
	for i := range 8 {
		wg.Go(func() {
			request := fmt.Sprintf("ow-req-%d", i)
			_ = store.UpdateSpares(func(ledger *Ledger, spares Spares) error {
				name, claimed := spares.Claim("f", request, epoch)
				if claimed {
					got <- [2]string{request, name}
				}
				return nil
			})
		})
	}
	wg.Wait()
	close(got)
	owners := map[string]string{}
	for pair := range got {
		if owner, ok := owners[pair[1]]; ok {
			t.Errorf("%s went to both %s and %s", pair[1], owner, pair[0])
		}
		owners[pair[1]] = pair[0]
	}
	if len(owners) != 3 {
		t.Errorf("%d of 3 spares were claimed by 8 requests", len(owners))
	}
	spares, err := store.ReadSpares()
	if err != nil {
		t.Fatal(err)
	}
	for id, owner := range owners {
		if spare := spares[id]; spare.State != Claimed || spare.Request != owner {
			t.Errorf("the ledger records %s as %+v, not claimed by %s", id, spare, owner)
		}
	}
}

func TestAHelperThatPredatesThePoolCannotEraseIt(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "ledger.json")}
	if err := store.UpdateSpares(func(ledger *Ledger, spares Spares) error {
		return spares.Prepare(ledger, "a", "sprites", "agents", "f", Rate{}, 0, 1, epoch)
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields["resources"] == nil {
		t.Errorf("the ledger carries %d fields; any helper that rewrites it must lose nothing of the pool", len(fields))
	}
	if err := store.Update(func(ledger *Ledger) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if spares, err := store.ReadSpares(); err != nil || spares["a"] == nil {
		t.Errorf("a ledger rewrite lost the spare: %v, %v", spares, err)
	}
}

func TestAWorkspaceDestroyedElsewhereLeavesThePool(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "ledger.json")}
	if err := store.UpdateSpares(func(ledger *Ledger, spares Spares) error {
		if err := spares.Prepare(ledger, "a", "sprites", "agents", "f", Rate{}, 0, 1, epoch); err != nil {
			return err
		}
		if err := spares.MarkReady(ledger, "a", epoch); err != nil {
			return err
		}
		spares.Claim("f", "ow-req", epoch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(ledger *Ledger) error { return ledger.Retire("a", epoch.Add(time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSpares(func(_ *Ledger, spares Spares) error {
		if len(spares) != 0 {
			t.Errorf("the next pool update still holds %v", spares)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if spares, err := store.ReadSpares(); err != nil || len(spares) != 0 {
		t.Errorf("the pool file keeps %v, %v", spares, err)
	}
}

func TestADrainThatDiedIsRetried(t *testing.T) {
	_, spares := pooled(t, "f", "a")
	if drained := spares.Drain(func(string, *Spare) bool { return true }); len(drained) != 1 {
		t.Fatalf("drained %v", drained)
	}
	if again := spares.Drain(func(_ string, spare *Spare) bool { return spare.State == Draining }); !slices.Equal(again, []string{"a"}) {
		t.Errorf("a spare left draining by a dead drain was not retried: %v", again)
	}
	if name, claimed := spares.Claim("f", "ow-req", epoch); claimed {
		t.Errorf("a draining spare %s was claimed", name)
	}
}
