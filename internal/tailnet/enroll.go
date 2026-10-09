package tailnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/state"
)

const logoutTimeout = 60 * time.Second

type Runner interface {
	Run(ctx context.Context, script string, stdin io.Reader) ([]byte, error)
}

type Enroller struct {
	Connect  func(context.Context) (*Client, error)
	Bindings Bindings
	Daemon   Daemon
	Log      *slog.Logger

	client *Client
}

func (e *Enroller) Client(ctx context.Context) (*Client, error) {
	if e.client != nil {
		return e.client, nil
	}
	client, err := e.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := client.Verify(ctx); err != nil {
		return nil, err
	}
	e.client = client
	return client, nil
}

func (e *Enroller) Enroll(ctx context.Context, held *state.Held, machine Runner) (*Node, error) {
	if _, err := machine.Run(ctx, e.Daemon.FreshScript(), nil); err != nil {
		return nil, err
	}
	return e.join(ctx, held, machine)
}

func (e *Enroller) join(ctx context.Context, held *state.Held, machine Runner) (*Node, error) {
	client, err := e.Client(ctx)
	if err != nil {
		return nil, err
	}
	bound := e.Bindings.Of(held)
	started := time.Now()
	intent := Binding{Unresolved: true}
	if err := bound.Transition(Binding{}, intent); err != nil {
		return nil, err
	}
	key, err := client.MintKey(ctx, held.Name)
	if err != nil {
		return nil, errors.Join(err, bound.Transition(intent, Binding{}))
	}
	status, enrolled := machine.Run(ctx, e.Daemon.EnrollScript(held.Name), strings.NewReader(key+"\n"))
	node, err := e.confirmed(ctx, held.Name, status)
	if enrolled != nil {
		err = enrolled
	}
	if err != nil {
		return e.recover(context.WithoutCancel(ctx), bound, machine, err)
	}
	if err := bound.Transition(intent, Binding{NodeID: node.NodeID}); err != nil {
		return nil, err
	}
	e.Log.Info("joined the tailnet", "resource", held.Name, "node", node.NodeID, "name", node.DNSName, "ip", node.IP, "mode", node.Mode, "took", time.Since(started).Round(time.Millisecond))
	return &node, nil
}

func (e *Enroller) confirmed(ctx context.Context, name string, status []byte) (Node, error) {
	client, err := e.Client(ctx)
	if err != nil {
		return Node{}, err
	}
	node, running, err := ParseStatus(status, client.Credential.Suffix)
	if err != nil {
		return Node{}, err
	}
	if !running {
		return node, ErrNoNode
	}
	err = client.Owns(ctx, node.NodeID, name)
	switch {
	case errors.Is(err, ErrNoNode):
		return node, ErrNoNode
	case err != nil:
		return node, fmt.Errorf("%s holds a tailnet node that is not its own: %w", name, err)
	}
	return node, nil
}

func (e *Enroller) daemonNode(ctx context.Context, name string, machine Runner) (Node, error) {
	status, err := machine.Run(ctx, e.Daemon.StatusScript(), nil)
	if err != nil {
		return Node{}, err
	}
	return e.confirmed(ctx, name, status)
}

func (e *Enroller) recover(ctx context.Context, bound *Bound, machine Runner, cause error) (*Node, error) {
	name := bound.Resource()
	node, err := e.daemonNode(ctx, name, machine)
	if err != nil {
		return nil, fmt.Errorf("enrolling %s in the tailnet: %w; whether the tailnet holds a node for it is unknown, so check the tailnet for hostname %s", name, cause, name)
	}
	if err := bound.Transition(Binding{Unresolved: true}, Binding{NodeID: node.NodeID}); err != nil {
		return nil, errors.Join(cause, err)
	}
	return &node, fmt.Errorf("enrolling %s in the tailnet: %w; the machine holds its node %s, which is recorded for cleanup", name, cause, node.NodeID)
}

func (e *Enroller) Reattach(ctx context.Context, held *state.Held, machine Runner, recorded *Node) (*Node, bool, error) {
	name := held.Name
	bound := e.Bindings.Of(held)
	binding, err := bound.Read()
	if err != nil {
		return nil, false, err
	}
	if recorded != nil && recorded.NodeID != binding.NodeID {
		e.Log.Warn("the record names a tailnet node the binding does not; the binding and the machine's daemon decide, and that node is never deleted by this id", "resource", name, "record", recorded.NodeID, "bound", binding)
	}
	node, err := e.daemonNode(ctx, name, machine)
	if err != nil && !errors.Is(err, ErrNoNode) {
		return nil, false, err
	}
	running := err == nil
	switch {
	case binding.Unresolved && running:
		if err := bound.Transition(binding, Binding{NodeID: node.NodeID}); err != nil {
			return nil, false, err
		}
	case binding.Unresolved:
		return nil, false, fmt.Errorf("refusing to resume %s: its last enrollment never reported a node, so check the tailnet for hostname %s", name, name)
	case binding.NodeID == "" && running:
		return nil, false, fmt.Errorf("refusing to resume %s: it holds tailnet node %s that no enrollment of this tool recorded", name, node.NodeID)
	case binding.NodeID == "":
		joined, err := e.join(ctx, held, machine)
		return joined, true, err
	case running && node.NodeID != binding.NodeID:
		return nil, false, fmt.Errorf("refusing to resume %s: it holds tailnet node %s but is bound to %s", name, node.NodeID, binding.NodeID)
	case !running:
		e.Log.Info("the tailnet no longer holds this workspace's node; enrolling again", "resource", name, "node", binding.NodeID)
		if err := e.depart(ctx, bound, machine, binding, node); err != nil {
			return nil, false, err
		}
		joined, err := e.join(ctx, held, machine)
		return joined, true, err
	}
	e.Log.Info("still on the tailnet", "resource", name, "node", node.NodeID, "name", node.DNSName, "ip", node.IP)
	return &node, false, nil
}

func (e *Enroller) Leave(ctx context.Context, held *state.Held, recorded *Node) error {
	bound := e.Bindings.Of(held)
	binding, err := bound.Read()
	if err != nil {
		return err
	}
	if recorded != nil && recorded.NodeID != binding.NodeID {
		e.Log.Warn("the record names a tailnet node the binding does not; the binding decides, and that node is never deleted by this id", "resource", held.Name, "record", recorded.NodeID, "bound", binding)
	}
	switch {
	case binding == Binding{}:
		return nil
	case binding.Unresolved:
		if err := bound.Transition(binding, Binding{}); err != nil {
			return err
		}
		e.Log.Info("this workspace's enrollment never registered a node, so it has none to remove; a node it made unseen is ephemeral and leaves with the machine", "resource", held.Name)
		return nil
	}
	return e.revoke(ctx, bound, binding)
}

func (e *Enroller) depart(ctx context.Context, bound *Bound, machine Runner, binding Binding, node Node) error {
	client, err := e.Client(ctx)
	if err != nil {
		return err
	}
	if err := client.Owns(ctx, binding.NodeID, bound.Resource()); err != nil && !errors.Is(err, ErrNoNode) {
		return fmt.Errorf("refusing to delete: %w", err)
	}
	if node.NodeID == binding.NodeID {
		logout, cancel := context.WithTimeout(ctx, logoutTimeout)
		defer cancel()
		if _, err := machine.Run(logout, e.Daemon.LogoutScript(), nil); err != nil {
			e.Log.Warn("could not log the workspace out of the tailnet; its node is deleted through the API instead", "resource", bound.Resource(), "err", err)
		}
	}
	return e.revoke(ctx, bound, binding)
}

func (e *Enroller) revoke(ctx context.Context, bound *Bound, binding Binding) error {
	name := bound.Resource()
	client, err := e.Client(ctx)
	if err != nil {
		return err
	}
	if err := client.DeleteNode(ctx, binding.NodeID, name); err != nil {
		return err
	}
	if err := bound.Transition(binding, Binding{}); err != nil {
		return err
	}
	e.Log.Info("left the tailnet", "resource", name, "node", binding.NodeID)
	return nil
}
