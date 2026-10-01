package cli_test

import (
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
)

func TestProxyIsAHiddenCommand(t *testing.T) {
	root := cli.NewRootCmd()
	proxy, _, err := root.Find([]string{"proxy"})
	if err != nil || proxy.Name() != "proxy" || !proxy.Hidden {
		t.Fatalf("Find(proxy) = %v, %v; want the hidden proxy command", proxy, err)
	}
	root.SetArgs([]string{"proxy", "--", "true"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Errorf("proxy -- true = %v", err)
	}
}
