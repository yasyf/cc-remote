package workspace

import (
	"context"
	"sync"
)

type lanes struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	first  sync.Once
	err    error
}

func newLanes(ctx context.Context) *lanes {
	ctx, cancel := context.WithCancelCause(ctx)
	return &lanes{ctx: ctx, cancel: cancel}
}

func (l *lanes) Go(run func(ctx context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	l.wg.Go(func() {
		defer close(done)
		if err := run(l.ctx); err != nil {
			l.first.Do(func() {
				l.err = err
				l.cancel(err)
			})
		}
	})
	return done
}

func (l *lanes) after(upstream ...<-chan struct{}) error {
	for _, done := range upstream {
		select {
		case <-done:
		case <-l.ctx.Done():
			return context.Cause(l.ctx)
		}
	}
	return context.Cause(l.ctx)
}

func (l *lanes) Wait() error {
	l.wg.Wait()
	l.cancel(nil)
	return l.err
}
