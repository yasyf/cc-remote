package cli

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

func (w *firstWorker) waitForRuntime(fragment string) {
	served := make(chan struct{})
	var once sync.Once
	w.provider.Handle = func(id string, cmd []string, stdin []byte) providers.Result {
		script := strings.Join(cmd, " ")
		switch {
		case serveCommand.MatchString(script):
			once.Do(func() { close(served) })
		case strings.Contains(script, fragment):
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				return providers.Result{Stderr: []byte(fragment + " ran before the runtime was started"), ExitCode: 1}
			}
		}
		return w.handle(id, cmd, stdin)
	}
}

func (w *firstWorker) servedPorts() []int {
	w.t.Helper()
	var ports []int
	for _, call := range w.provider.Calls() {
		serve := serveCommand.FindStringSubmatch(call)
		if serve == nil {
			continue
		}
		port, err := strconv.Atoi(serve[1])
		if err != nil {
			w.t.Fatal(err)
		}
		ports = append(ports, port)
	}
	return ports
}

func TestACreatedLaneStartsItsRuntimeWhileTheCreateConfigures(t *testing.T) {
	w := newFirstWorker(t, nil, false)
	w.waitForRuntime("plugins.sh configure")
	native, commands := w.native()
	var out bytes.Buffer
	if err := w.prepare(t.Context(), w.session, native, &out, "task-a", false); err != nil {
		t.Fatal(err)
	}
	w.head = gitOutput(t, w.root, "rev-parse", "HEAD")
	task := w.prepared(out.Bytes())
	if ports := w.servedPorts(); len(ports) != 1 || ports[0] != task.Port || task.RuntimeID != "rt-1" {
		t.Errorf("runtime starts on ports %v for a task on port %d with runtime %q; want one start on the task's port", ports, task.Port, task.RuntimeID)
	}
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant\n%q", native.calls, commands)
	}
	w.free("task-a")
}

func TestAWarmClaimStartsItsRuntimeWhileItRefreshesTheCheckout(t *testing.T) {
	w := newWarmPool(t, "pool-a")
	standInFill(t)
	if _, err := openPool(w.session).fill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ports := w.servedPorts(); len(ports) != 0 {
		t.Fatalf("the fill started a runtime on ports %v", ports)
	}
	t.Setenv("REMOTE_HOME", w.local.Home("pool-a"))
	w.waitForRuntime("fetch --quiet --prune --depth 1 origin")
	native, commands := w.nativeAs("pool-a")
	var out bytes.Buffer
	if err := w.warm(t.Context(), w.session, native, &out, "lane-a", ""); err != nil {
		t.Fatal(err)
	}
	task := w.allocation(out.Bytes()).Task
	if ports := w.servedPorts(); task == nil || len(ports) != 1 || ports[0] != task.Port || task.RuntimeID != "rt-1" {
		t.Errorf("runtime starts on ports %v for task %+v; want one start on the task's port", ports, task)
	}
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant\n%q", native.calls, commands)
	}
	w.free("pool-a")
	w.free("lane-a")
}
