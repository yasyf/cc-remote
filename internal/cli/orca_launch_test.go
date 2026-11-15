package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestALaunchStopsOnceTheProcessThatStartedItExits(t *testing.T) {
	original := parentEvery
	parentEvery = 10 * time.Millisecond
	t.Cleanup(func() { parentEvery = original })
	tests := []struct {
		name    string
		started int32
		after   int32
		want    error
	}{
		{"its parent exits", 4242, 1, errOrphaned},
		{"its parent stays", 4242, 4242, nil},
		{"it started detached", 1, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parent atomic.Int32
			parent.Store(tt.started)
			ctx, cancel := launchContext(t.Context(), func() int { return int(parent.Load()) })
			defer cancel()
			parent.Store(tt.after)
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			if got := context.Cause(ctx); !errors.Is(got, tt.want) {
				t.Errorf("cause = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestALaunchHasADeadline(t *testing.T) {
	ctx, cancel := launchContext(t.Context(), func() int { return 1 })
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > launchWithin || time.Until(deadline) < launchWithin-time.Minute {
		t.Errorf("deadline = %v, %v, want %s from now", deadline, ok, launchWithin)
	}
}
