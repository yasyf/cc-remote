package sprites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type downloadRunner struct {
	*fakeSprites
	body     string
	probe    string
	short    bool
	stderr   string
	exitCode int
	reads    [][]string
}

func (r *downloadRunner) Run(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	if len(cmd.Args) < 4 || !strings.Contains(cmd.Args[3], "/fs/read") {
		return r.fakeSprites.Run(ctx, cmd)
	}
	if cmd.Name != r.cli || cmd.Stdin != nil {
		r.t.Fatalf("download ran %q with stdin %v", cmd.Name, cmd.Stdin)
	}
	r.reads = append(r.reads, slices.Clone(cmd.Args[4:]))
	if r.exitCode != 0 {
		return providers.Result{Stderr: []byte(r.stderr), ExitCode: r.exitCode}, nil
	}
	ranged := cmd.Args[slices.Index(cmd.Args, "-r")+1]
	if ranged == "0-0" && slices.Contains(cmd.Args, "-w") {
		return providers.Result{Stdout: []byte(r.probe)}, nil
	}
	var start, end int64
	if _, err := fmt.Sscanf(ranged, "%d-%d", &start, &end); err != nil {
		r.t.Fatalf("download asked for range %q: %v", ranged, err)
	}
	n := end - start + 1
	if r.short {
		n--
	}
	if r.body != "" {
		_, err := io.WriteString(cmd.Stdout, r.body[start:start+n])
		return providers.Result{}, err
	}
	_, err := io.CopyN(cmd.Stdout, zeros{}, n)
	return providers.Result{}, err
}

func TestDownloadReadsAFileThroughTheAPIInRanges(t *testing.T) {
	const payload = "/var/lib/cc-remote/build/payload.sqfs"
	probe := []string{"--", "-sS", "-f", "-r", "0-0", "-o", "/dev/null", "-w", "%header{content-range}"}
	ranged := func(r string) []string { return []string{"--", "-sS", "-f", "-r", r} }
	tests := []struct {
		name     string
		sprite   string
		body     string
		size     int64
		probe    string
		short    bool
		stderr   string
		exitCode int
		reads    [][]string
		is       error
		message  string
	}{
		{name: "reads a small file in one range", sprite: "s1", body: "hsqs payload bytes", probe: "bytes 0-0/18", reads: [][]string{probe, ranged("0-17")}},
		{
			name: "reads a file past the endpoint's 2 GiB response cap in 1 GiB ranges", sprite: "s1", size: 2154352640, probe: "bytes 0-0/2154352640",
			reads: [][]string{probe, ranged("0-1073741823"), ranged("1073741824-2147483647"), ranged("2147483648-2154352639")},
		},
		{
			name: "fails on a range that arrives short", sprite: "s1", body: "hsqs payload bytes", probe: "bytes 0-0/18", short: true, reads: [][]string{probe, ranged("0-17")},
			message: "downloading " + payload + " from sprite s1: bytes 0-17 of 18 arrived as 17 bytes, want 18",
		},
		{
			name: "fails on a probe that names no size", sprite: "s1", probe: "", reads: [][]string{probe},
			message: "downloading " + payload + ` from sprite s1: its first byte answered content-range "", not bytes 0-0/<size>`,
		},
		{
			name: "fails on an HTTP error", sprite: "s1", stderr: "curl: (22) The requested URL returned error: 404", exitCode: 22, reads: [][]string{probe},
			message: "downloading " + payload + " from sprite s1: <cli> api -o acme /v1/sprites/s1/fs/read?path=%2Fvar%2Flib%2Fcc-remote%2Fbuild%2Fpayload.sqfs " + strings.Join(probe, " ") + ": exit 22: curl: (22) The requested URL returned error: 404",
		},
		{name: "refuses a missing sprite", sprite: "gone", is: providers.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			if _, err := p.Create(t.Context(), spec("s1", nil)); err != nil {
				t.Fatal(err)
			}
			runner := &downloadRunner{fakeSprites: fake, body: tt.body, probe: tt.probe, short: tt.short, stderr: tt.stderr, exitCode: tt.exitCode}
			p.Runner = runner
			var out bytes.Buffer
			var written byteCount
			w := io.Writer(&out)
			if tt.size > 0 {
				w = &written
			}
			err := p.Download(t.Context(), tt.sprite, payload, w)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) {
					t.Errorf("Download = %v, want %v", err, tt.is)
				}
			case tt.message != "":
				if want := strings.Replace(tt.message, "<cli>", p.CLI, 1); err == nil || err.Error() != want {
					t.Errorf("Download = %v, want %q", err, want)
				}
			case err != nil:
				t.Errorf("Download = %v", err)
			}
			if tt.size > 0 && int64(written) != tt.size {
				t.Errorf("wrote %d bytes, want %d", written, tt.size)
			}
			if tt.size == 0 && tt.message == "" && out.String() != tt.body {
				t.Errorf("wrote %q, want %q", out.String(), tt.body)
			}
			if !slices.EqualFunc(runner.reads, tt.reads, slices.Equal) {
				t.Errorf("reads = %q, want %q", runner.reads, tt.reads)
			}
		})
	}
}
