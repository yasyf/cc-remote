package namespace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"

	"github.com/yasyf/cc-remote/internal/providers"
)

type Lease struct {
	InstanceID  string    `json:"instanceId"`
	Status      string    `json:"status"`
	Deadline    time.Time `json:"deadline"`
	Horizon     string    `json:"horizon"`
	Granted     string    `json:"granted"`
	CheckedAt   time.Time `json:"checkedAt"`
	RenewedAt   time.Time `json:"renewedAt,omitzero"`
	NextCheckAt time.Time `json:"nextCheckAt,omitzero"`
	Error       string    `json:"error,omitempty"`
	Stopped     bool      `json:"stopped,omitempty"`
}

type keeper struct {
	horizon time.Duration
	granted time.Duration
	lease   Lease
}

func (p *Provider) Keep(ctx context.Context, id string, observe func(Lease)) error {
	saved, err := p.load(id)
	if err != nil {
		return err
	}
	if saved == nil {
		return fmt.Errorf("namespace instance %s: %w", id, providers.ErrNotFound)
	}
	k := &keeper{horizon: p.Duration, granted: p.Duration, lease: Lease{InstanceID: id, Horizon: p.Duration.String(), Deadline: saved.Instance.Deadline}}
	for {
		started := p.Now().UTC()
		k.lease.CheckedAt, k.lease.Error = started, ""
		if p.check(ctx, *saved, k) {
			observe(k.lease)
			return nil
		}
		wait := k.schedule(p.Now().UTC(), started)
		observe(k.lease)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.After(wait):
		}
	}
}

func (k *keeper) fail(format string, args ...any) {
	k.lease.Error = strings.TrimPrefix(k.lease.Error+"; "+fmt.Sprintf(format, args...), "; ")
}

func (p *Provider) check(ctx context.Context, saved instance, k *keeper) bool {
	described, err := p.describe(ctx, saved)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		k.lease.Status, k.lease.Stopped = computev1beta.InstanceMetadata_DESTROYED.String(), true
		return true
	case err != nil:
		k.fail("reading the lease failed, so the deadline last seen, %s, still governs: %v", k.lease.Deadline.Format(time.RFC3339), err)
		return false
	}
	metadata := described.GetMetadata()
	k.lease.Status = metadata.GetStatus().String()
	if deadline := metadata.GetDeadline(); deadline != nil {
		k.lease.Deadline = deadline.AsTime().UTC()
	}
	if k.lease.Deadline.Sub(p.Now()) > k.granted/2 {
		return false
	}
	previous, asked := k.lease.Deadline, p.Now().UTC()
	renewed, err := p.renew(ctx, saved, k.horizon)
	switch {
	case err != nil:
		k.fail("renewing the %s instance to at least %s left failed, so it still expires at %s: %v", k.lease.Status, k.horizon, previous.Format(time.RFC3339), err)
	case !renewed.After(previous) || !renewed.After(asked):
		k.fail("renewing the %s instance answered deadline %s, which did not move past %s", k.lease.Status, renewed.Format(time.RFC3339), previous.Format(time.RFC3339))
	default:
		k.lease.Deadline, k.lease.RenewedAt, k.granted = renewed, asked, renewed.Sub(asked)
		if renewed.Before(asked.Add(k.horizon)) {
			k.fail("renewing the %s instance answered deadline %s, %s from the request and short of the requested %s", k.lease.Status, renewed.Format(time.RFC3339), k.granted, k.horizon)
		}
	}
	return false
}

func (k *keeper) schedule(now, started time.Time) time.Duration {
	remaining, spent, lead := k.lease.Deadline.Sub(now), now.Sub(started), k.granted/2
	k.lease.Granted = k.granted.String()
	var wait time.Duration
	switch {
	case remaining <= 0:
		k.fail("the deadline %s has passed", k.lease.Deadline.Format(time.RFC3339))
		wait = lead
	case remaining <= spent:
		k.fail("the %s left before the deadline %s is no longer than the %s one check took, so the next renewal may land after it", remaining, k.lease.Deadline.Format(time.RFC3339), spent)
	case remaining > lead:
		wait = remaining - max(lead, spent)
	default:
		wait = min(remaining/2, remaining-spent)
	}
	k.lease.NextCheckAt = now.Add(wait)
	return wait
}

func (p *Provider) renew(ctx context.Context, saved instance, minimum time.Duration) (time.Time, error) {
	var extended *computev1beta.ExtendInstanceResponse
	if err := p.call(ctx, saved.Instance.Endpoint, func(ctx context.Context, compute computev1beta.ComputeServiceClient) (err error) {
		extended, err = compute.ExtendInstance(ctx, &computev1beta.ExtendInstanceRequest{InstanceId: saved.Instance.InstanceID, EnsureMinimum: durationpb.New(minimum)})
		return err
	}); err != nil {
		return time.Time{}, err
	}
	if extended.GetNewDeadline() == nil {
		return time.Time{}, errors.New("the renewal answered without its new deadline")
	}
	deadline := extended.GetNewDeadline().AsTime().UTC()
	saved.Instance.Deadline = deadline
	return deadline, p.save(saved)
}
