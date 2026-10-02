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

func (l *lanes) Go(run func(ctx context.Context) error) {
	l.wg.Go(func() {
		if err := run(l.ctx); err != nil {
			l.first.Do(func() {
				l.err = err
				l.cancel(err)
			})
		}
	})
}

func (l *lanes) Wait() error {
	l.wg.Wait()
	l.cancel(nil)
	return l.err
}
