package cli_test

import (
	"bytes"
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
	"github.com/yasyf/cc-remote/internal/version"
)

func TestVersion(t *testing.T) {
	tests := []struct {
		name    string
		stamped string
		want    string
	}{
		{name: "unstamped build prints dev", stamped: "dev", want: "dev\n"},
		{name: "ldflags stamp wins", stamped: "v1.2.3", want: "v1.2.3\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := version.Version
			version.Version = tt.stamped
			t.Cleanup(func() { version.Version = original })

			var out bytes.Buffer
			root := cli.NewRootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"version"})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}
