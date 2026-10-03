package namespace

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
)

func keep(t *testing.T, p *Provider, clock *fakeClock, rounds int, between func(round int)) ([]Lease, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.After = func(wait time.Duration) <-chan time.Time {
		if ctx.Err() != nil {
			return nil
		}
		if wait < 0 {
			t.Errorf("the keeper scheduled a negative wait %s", wait)
		}
		clock.advance(wait)
		ready := make(chan time.Time, 1)
		ready <- clock.Now()
		return ready
	}
	var leases []Lease
	err := p.Keep(ctx, "inst1", func(lease Lease) {
		leases = append(leases, lease)
		if len(leases) == rounds {
			cancel()
			return
		}
		if between != nil {
			between(len(leases))
		}
	})
	return leases, err
}

func at(offset time.Duration) time.Time { return epoch.Add(offset) }

func checks(leases []Lease) []time.Time {
	var times []time.Time
	for _, lease := range leases {
		times = append(times, lease.CheckedAt)
	}
	return times
}

func TestKeepRenewsAShortHorizonBeforeItExpires(t *testing.T) {
	p, fake, clock := newProvider(t)
	p.Duration = time.Minute
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	clock.advance(5 * time.Second)
	leases, err := keep(t, p, clock, 2, nil)
	if !errors.Is(err, context.Canceled) || len(leases) != 2 {
		t.Fatalf("Keep = %v after %d leases", err, len(leases))
	}
	first, second := leases[0], leases[1]
	if !first.Deadline.Equal(at(time.Minute)) || !first.RenewedAt.IsZero() || !first.NextCheckAt.Equal(at(30*time.Second)) || first.Error != "" {
		t.Errorf("first lease = %+v, want no renewal with 55s left and a next check at 30s", first)
	}
	if !second.RenewedAt.Equal(at(30*time.Second)) || !second.Deadline.Equal(at(90*time.Second)) || second.Granted != "1m0s" || second.Error != "" {
		t.Errorf("second lease = %+v, want a renewal at 30s, before the 60s deadline, to 90s", second)
	}
	if len(fake.extended) != 1 || fake.extended[0].GetEnsureMinimum().AsDuration() != time.Minute || fake.extended[0].GetExtendBy() != nil {
		t.Errorf("extend requests = %v, want one ensure_minimum of the configured 1m", fake.extended)
	}
	if saved, err := p.load("inst1"); err != nil || !saved.Instance.Deadline.Equal(at(90*time.Second)) {
		t.Errorf("recorded deadline = %+v, %v", saved, err)
	}
}

func TestKeepSchedulesAClampedLeaseFromItsActualGrant(t *testing.T) {
	p, fake, clock := newProvider(t)
	fake.limit = 15 * time.Minute
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	leases, err := keep(t, p, clock, 6, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	want := []time.Time{at(0), at(450 * time.Second), at(15 * time.Minute), at(1350 * time.Second), at(30 * time.Minute), at(2250 * time.Second)}
	if got := checks(leases); !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Errorf("checks = %v, want one every 7m30s rather than every minute", got)
	}
	if !strings.Contains(leases[0].Error, "which did not move past 2026-10-03T09:15:00Z") {
		t.Errorf("first lease error = %q, want the unchanged clamp reported", leases[0].Error)
	}
	for _, lease := range leases[1:] {
		if lease.Granted != "15m0s" || !strings.Contains(lease.Error, "short of the requested 4h0m0s") || !lease.Deadline.Equal(lease.RenewedAt.Add(15*time.Minute)) {
			t.Errorf("lease = %+v, want a 15m grant reported short of the 4h request", lease)
		}
	}
	for _, request := range fake.extended {
		if request.GetEnsureMinimum().AsDuration() != 4*time.Hour {
			t.Errorf("extend request %v, want the configured 4h horizon kept as the request", request)
		}
	}
}

func TestKeepNeverSleepsPastAMinuteLongGrant(t *testing.T) {
	p, fake, clock := newProvider(t)
	fake.limit = time.Minute
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	leases, err := keep(t, p, clock, 5, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i, lease := range leases {
		if !lease.NextCheckAt.Before(lease.Deadline) {
			t.Errorf("lease %d = %+v sleeps until its deadline or past it", i, lease)
		}
		if i > 0 && !lease.CheckedAt.Before(leases[i-1].Deadline) {
			t.Errorf("check %d at %v came after the deadline %v", i, lease.CheckedAt, leases[i-1].Deadline)
		}
	}
	if want := []time.Time{at(0), at(30 * time.Second), at(time.Minute), at(90 * time.Second), at(2 * time.Minute)}; !slices.EqualFunc(checks(leases), want, time.Time.Equal) {
		t.Errorf("checks = %v, want %v", checks(leases), want)
	}
}

func TestKeepCountsTheTimeItsCallsTake(t *testing.T) {
	p, fake, clock := newProvider(t)
	p.Duration = time.Minute
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.latency = 10 * time.Second
	leases, err := keep(t, p, clock, 3, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	want := []struct {
		checked, renewed, deadline, next time.Duration
	}{
		{0, -1, time.Minute, 30 * time.Second},
		{30 * time.Second, 40 * time.Second, 100 * time.Second, 70 * time.Second},
		{70 * time.Second, 80 * time.Second, 140 * time.Second, 110 * time.Second},
	}
	for i, w := range want {
		lease := leases[i]
		renewed := lease.RenewedAt.IsZero() && w.renewed < 0 || lease.RenewedAt.Equal(at(w.renewed))
		if !lease.CheckedAt.Equal(at(w.checked)) || !renewed || !lease.Deadline.Equal(at(w.deadline)) || !lease.NextCheckAt.Equal(at(w.next)) {
			t.Errorf("lease %d = %+v, want checked %s, renewed %s, deadline %s, next %s", i, lease, w.checked, w.renewed, w.deadline, w.next)
		}
	}
}

func TestKeepReportsAnIntervalShorterThanOneCheck(t *testing.T) {
	p, fake, clock := newProvider(t)
	fake.limit = 15 * time.Second
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.latency = 10 * time.Second
	leases, err := keep(t, p, clock, 1, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	lease := leases[0]
	if !strings.Contains(lease.Error, "short of the requested 4h0m0s") || !strings.Contains(lease.Error, "the 5s left before the deadline 2026-10-03T09:00:25Z is no longer than the 20s one check took") {
		t.Errorf("lease error = %q, want the short grant and the interval limitation", lease.Error)
	}
	if !lease.NextCheckAt.Equal(lease.CheckedAt.Add(20 * time.Second)) {
		t.Errorf("next check = %v, want an immediate retry after the 20s check", lease.NextCheckAt)
	}
}

func TestKeepRetriesAnUnmovedOrFailedRenewalBeforeTheDeadline(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeNamespace)
		want  string
	}{
		{"unchanged", func(f *fakeNamespace) { f.clamp = at(4 * time.Hour) }, "renewing the RUNNING instance answered deadline 2026-10-03T13:00:00Z, which did not move past 2026-10-03T13:00:00Z"},
		{"denied", func(f *fakeNamespace) { f.extendErr = status.Error(codes.PermissionDenied, "denied") }, "renewing the RUNNING instance to at least 4h0m0s left failed, so it still expires at 2026-10-03T13:00:00Z: rpc error: code = PermissionDenied desc = denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, clock := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			tt.setup(fake)
			clock.advance(2 * time.Hour)
			leases, err := keep(t, p, clock, 3, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if want := []time.Time{at(2 * time.Hour), at(3 * time.Hour), at(210 * time.Minute)}; !slices.EqualFunc(checks(leases), want, time.Time.Equal) {
				t.Errorf("checks = %v, want retries halving the time left", checks(leases))
			}
			for _, lease := range leases {
				if lease.Error != tt.want || !lease.Deadline.Equal(at(4*time.Hour)) {
					t.Errorf("lease = %+v, want error %q with the unchanged deadline", lease, tt.want)
				}
			}
		})
	}
}

func TestKeepRenewsOrReportsASuspendedInstance(t *testing.T) {
	tests := []struct {
		name     string
		denied   bool
		deadline time.Time
		want     string
	}{
		{"extended", false, at(6 * time.Hour), ""},
		{"denied", true, at(4 * time.Hour), "renewing the SUSPENDED instance to at least 4h0m0s left failed, so it still expires at 2026-10-03T13:00:00Z: rpc error: code = FailedPrecondition desc = suspended instances keep their deadline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake, clock := newProvider(t)
			if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
				t.Fatal(err)
			}
			if err := p.Suspend(t.Context(), "inst1"); err != nil {
				t.Fatal(err)
			}
			if tt.denied {
				fake.extendErr = status.Error(codes.FailedPrecondition, "suspended instances keep their deadline")
			}
			clock.advance(2 * time.Hour)
			leases, err := keep(t, p, clock, 2, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			for _, lease := range leases {
				if lease.Status != "SUSPENDED" || lease.Error != tt.want || lease.Stopped {
					t.Errorf("lease = %+v, want the suspended instance kept with error %q", lease, tt.want)
				}
			}
			if !leases[0].Deadline.Equal(tt.deadline) {
				t.Errorf("deadline = %v, want %v", leases[0].Deadline, tt.deadline)
			}
			if slices.ContainsFunc(fake.Calls(), func(call string) bool { return strings.HasPrefix(call, "wake") || strings.HasPrefix(call, "destroy") }) {
				t.Errorf("calls = %q; the keeper woke or destroyed the instance", fake.Calls())
			}
		})
	}
}

func TestKeepStopsOnceTheInstanceIsDestroyed(t *testing.T) {
	p, fake, clock := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.set("inst1", computev1beta.InstanceMetadata_DESTROYED)
	leases, err := keep(t, p, clock, 2, nil)
	if err != nil || len(leases) != 1 || !leases[0].Stopped || leases[0].Status != "DESTROYED" {
		t.Errorf("Keep = %v with %+v, want one stopped lease", err, leases)
	}
}

func TestKeepGovernsByTheLastDeadlineWhenReadingFails(t *testing.T) {
	p, fake, clock := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	fake.describeErr = status.Error(codes.Unavailable, "try again")
	leases, err := keep(t, p, clock, 2, func(int) { fake.describeErr = nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if want := "reading the lease failed, so the deadline last seen, 2026-10-03T13:00:00Z, still governs: rpc error: code = Unavailable desc = try again"; leases[0].Error != want || !leases[0].NextCheckAt.Equal(at(2*time.Hour)) {
		t.Errorf("first lease = %+v, want %q and a check when the renewal window opens", leases[0], want)
	}
	if !leases[1].RenewedAt.Equal(at(2*time.Hour)) || !leases[1].Deadline.Equal(at(6*time.Hour)) || leases[1].Error != "" {
		t.Errorf("second lease = %+v, want a renewal once reading recovered", leases[1])
	}
}
