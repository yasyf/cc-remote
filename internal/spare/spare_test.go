package spare

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func ready(t *testing.T, names ...string) Spares {
	t.Helper()
	spares := Spares{}
	for i, name := range names {
		if err := spares.Prepare(name, "sprites", "lean", "f", os.Getpid()); err != nil {
			t.Fatal(err)
		}
		if err := spares.MarkReady(name, epoch.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return spares
}

func TestFreshStoreNeedsNoInitialization(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "spares.json")}
	spares, err := store.Read()
	if err != nil || len(spares) != 0 {
		t.Fatalf("Read = %v, %v", spares, err)
	}
	if err := store.Update(func(spares Spares) error { return spares.Prepare("a", "sprites", "lean", "f", os.Getpid()) }); err != nil {
		t.Fatal(err)
	}
	spares, err = store.Read()
	if err != nil || spares["a"].Provider != "sprites" || spares["a"].Profile != "lean" || spares["a"].State != Preparing {
		t.Fatalf("Read = %v, %v", spares, err)
	}
}

func TestRejectedUpdateLeavesExistingStateUnchanged(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "spares.json")}
	if err := store.Update(func(spares Spares) error { return spares.Prepare("a", "sprites", "lean", "f", os.Getpid()) }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	if err := store.Update(func(spares Spares) error { delete(spares, "a"); return refused }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed update changed state: %s, %v", after, err)
	}
}

func TestClaimTakesTheLongestReadyMatchingSpare(t *testing.T) {
	spares := ready(t, "a", "b", "c")
	spares["a"].Fingerprint = "old"
	if name, claimed := spares.Claim("f", "ws-1", epoch); !claimed || name != "b" {
		t.Fatalf("Claim = %s, %v", name, claimed)
	}
	if name, claimed := spares.Claim("f", "ws-1", epoch); !claimed || name != "b" {
		t.Fatalf("retry = %s, %v", name, claimed)
	}
	if name, claimed := spares.Claim("f", "ws-2", epoch); !claimed || name != "c" {
		t.Fatalf("next = %s, %v", name, claimed)
	}
	if name, claimed := spares.Claim("f", "ws-3", epoch); claimed || name != "ws-3" {
		t.Fatalf("empty = %s, %v", name, claimed)
	}
}

func TestActivationRequiresAClaim(t *testing.T) {
	spares := ready(t, "a")
	if err := spares.Activate("a", epoch); err == nil {
		t.Fatal("activated an unclaimed spare")
	}
	spares.Claim("f", "ws-1", epoch)
	if err := spares.Activate("a", epoch); err != nil {
		t.Fatal(err)
	}
	if spares["a"].ActivatedAt == nil || !spares["a"].ActivatedAt.Equal(epoch) {
		t.Fatalf("activation = %+v", spares["a"])
	}
}

func TestClaimSkipsPreparingAndStaleSpares(t *testing.T) {
	spares := ready(t, "old")
	spares["old"].Fingerprint = "old"
	if err := spares.Prepare("preparing", "sprites", "lean", "f", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if name, claimed := spares.Claim("f", "ws-1", epoch); claimed || name != "ws-1" {
		t.Fatalf("Claim = %s, %v", name, claimed)
	}
	if spares.Pooled("f") != 1 || spares["preparing"].State != Preparing || spares["old"].State != Ready {
		t.Fatalf("spares = %+v", spares)
	}
}

func TestPreparationCannotReplaceAnExistingSpare(t *testing.T) {
	spares := ready(t, "a")
	before := *spares["a"]
	if err := spares.Prepare("a", "namespace", "stack", "other", os.Getpid()); err == nil {
		t.Fatal("preparation replaced an existing spare")
	}
	if *spares["a"] != before {
		t.Fatalf("spare changed to %+v", spares["a"])
	}
	if err := spares.MarkReady("a", epoch); err == nil {
		t.Fatal("a ready spare was marked ready again")
	}
}

func TestDrainLeavesClaimsAndRetriesUnfinishedDrains(t *testing.T) {
	spares := ready(t, "a", "b", "c")
	spares.Claim("f", "ws-1", epoch)
	if got := spares.Drain(func(id string, _ *Spare) bool { return id != "c" }); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Drain = %v", got)
	}
	if got := spares.Drain(func(_ string, item *Spare) bool { return item.State == Draining }); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("retry = %v", got)
	}
	if name, claimed := spares.Claim("f", "ws-2", epoch); !claimed || name != "c" {
		t.Fatalf("Claim = %s, %v", name, claimed)
	}
}

func TestConcurrentClaimsNeverShareASpare(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "spares.json")}
	if err := store.Update(func(spares Spares) error {
		for id, item := range ready(t, "a", "b", "c") {
			spares[id] = item
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claimed := make(chan [2]string, 8)
	for i := range 8 {
		wg.Go(func() {
			request := fmt.Sprintf("ws-%d", i)
			if err := store.Update(func(spares Spares) error {
				name, ok := spares.Claim("f", request, epoch)
				if ok {
					claimed <- [2]string{request, name}
				}
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(claimed)
	owners := map[string]string{}
	for claim := range claimed {
		if owner, seen := owners[claim[1]]; seen {
			t.Errorf("%s claimed by both %s and %s", claim[1], owner, claim[0])
		}
		owners[claim[1]] = claim[0]
	}
	if len(owners) != 3 {
		t.Fatalf("claims = %v", owners)
	}
	spares, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for id, owner := range owners {
		if item := spares[id]; item.State != Claimed || item.Request != owner {
			t.Errorf("spare %s = %+v, want claimed by %s", id, item, owner)
		}
	}
}
