package orca_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type reply struct {
	out string
	err error
}

type fakeOrca struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string][]reply
	calls   []string
}

func newFakeOrca(t *testing.T) *fakeOrca {
	return &fakeOrca{t: t, replies: map[string][]reply{}}
}

func (f *fakeOrca) on(command, out string) *fakeOrca {
	f.replies[command] = append(f.replies[command], reply{out: out})
	return f
}

func (f *fakeOrca) fail(command, out string, err error) *fakeOrca {
	f.replies[command] = append(f.replies[command], reply{out: out, err: err})
	return f
}

func (f *fakeOrca) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	command := strings.Join(args, " ")
	f.calls = append(f.calls, command)
	queue := f.replies[command]
	if len(queue) == 0 {
		f.t.Errorf("unexpected orca call: %s", command)
		return nil, fmt.Errorf("unexpected orca call: %s", command)
	}
	next := queue[0]
	if len(queue) > 1 {
		f.replies[command] = queue[1:]
	}
	return []byte(next.out), next.err
}

func (f *fakeOrca) called(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == command {
			n++
		}
	}
	return n
}
