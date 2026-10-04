package cli

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/remote"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	orcaReportSchema   = 1
	taskRoot           = remote.StateDir + "/orca/tasks"
	briefName          = "brief.md"
	statusLimit        = 64 << 10
	exitInsideCheckout = 3
	exitNotArtifact    = 4
	exitNotCheckout    = 5
	exitOtherOrigin    = 6
	exitPriorRuntime   = 7
	exitNotClean       = 8
)

var (
	commitID         = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	digestHex        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	transferRefusals = map[int]string{
		exitInsideCheckout: "the task directory resolves inside the checkout",
		exitNotArtifact:    "the path is not a regular file directly in the recorded physical task directory",
	}
	checkoutRefusals = map[int]string{
		exitNotCheckout:  "the configured project root is not the top level of a git checkout",
		exitOtherOrigin:  "the checkout's origin is not the configured repository",
		exitPriorRuntime: "the machine already holds cc-remote Orca runtime state, which a first worker neither adopts nor rewrites",
		exitNotClean:     "the checkout has uncommitted or untracked changes, so it is not unused",
	}
)

type orcaArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type orcaReport struct {
	SchemaVersion int          `json:"schemaVersion"`
	BaseCommit    string       `json:"baseCommit"`
	Patch         orcaArtifact `json:"patch"`
	Files         []struct {
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
		Deleted bool   `json:"deleted"`
	} `json:"files"`
}

type orcaChange struct {
	Status string `json:"status"`
	Path   string `json:"path"`
	From   string `json:"from,omitempty"`
}

type orcaStatus struct {
	Changes  []orcaChange `json:"changes"`
	Bytes    int          `json:"bytes"`
	Overflow bool         `json:"overflow"`
}

type orcaCollection struct {
	Workspace  string       `json:"workspace"`
	BaseCommit string       `json:"baseCommit"`
	Report     orcaArtifact `json:"report"`
	Patch      orcaArtifact `json:"patch"`
	Status     orcaStatus   `json:"status"`
}

type orcaCollect struct{ report, patch, output string }

func artifactOf(name string, data []byte) orcaArtifact {
	sum := sha256.Sum256(data)
	return orcaArtifact{Path: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))}
}

func (t *orcaTask) fetch(ctx context.Context, step, script string, stdin io.Reader) ([]byte, error) {
	result, err := t.shell(ctx, script, stdin)
	return answered(step, t.Workspace, transferRefusals, result, err)
}

func answered(step, name string, refusals map[int]string, result providers.Result, err error) ([]byte, error) {
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %w", step, name, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("%s on %s exited %d: %s", step, name, result.ExitCode, cmp.Or(refusals[result.ExitCode], "its remote output is withheld"))
	}
	return result.Stdout, nil
}

func checkRetained(ctx context.Context, session *workspace.Session, result *workspace.Result) error {
	return checkCheckout(ctx, session, "check the retained checkout", result.Name, result.Machine, result.ProjectRoot)
}

func checkUnused(ctx context.Context, session *workspace.Session, record *workspace.Record) error {
	return checkCheckout(ctx, session, "check the unused checkout", record.Name, record.Machine, session.ProjectRoot(),
		`status=$(git --no-optional-locks -C "$root" status --porcelain --untracked-files=all)`,
		`test -z "$status" || exit `+fmt.Sprint(exitNotClean),
	)
}

func checkCheckout(ctx context.Context, session *workspace.Session, step, name, machine, root string, checks ...string) error {
	script := remote.Script(slices.Concat([]string{
		"root=" + remote.Quote(root),
		`top=$(git -C "$root" rev-parse --show-toplevel 2> /dev/null) && test "$top" = "$(cd "$root" && pwd -P)" || exit ` + fmt.Sprint(exitNotCheckout),
		`test "$(git -C "$root" config --get remote.origin.url)" = ` + remote.Quote(session.Config.Repository) + ` || exit ` + fmt.Sprint(exitOtherOrigin),
		orca.FirstUseCheck + ` || exit ` + fmt.Sprint(exitPriorRuntime),
	}, checks)...)
	ran, err := session.Provider.Exec(ctx, machine, []string{"sh", "-c", script}, nil)
	_, err = answered(step, name, checkoutRefusals, ran, err)
	return err
}

func readBrief(file string) ([]byte, error) {
	brief, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read the brief: %w", err)
	}
	if len(bytes.TrimSpace(brief)) == 0 {
		return nil, fmt.Errorf("the brief in %s is empty", file)
	}
	return brief, nil
}

func (d orcaDriver) publish(ctx context.Context, task *orcaTask, brief []byte) error {
	started := time.Now()
	base, err := task.head(ctx)
	d.observe("task.head", started, err)
	if err != nil {
		return err
	}
	task.BaseCommit = base
	if err := d.save(task); err != nil {
		return err
	}
	started = time.Now()
	task.Brief, err = task.sendBrief(ctx, brief)
	d.observe("task.sendBrief", started, err)
	if err != nil {
		return err
	}
	task.Prepared = true
	return d.save(task)
}

func (t *orcaTask) head(ctx context.Context) (string, error) {
	out, err := t.fetch(ctx, "read the checkout's HEAD", "exec git -C "+remote.Quote(t.ProjectRoot)+" rev-parse --verify HEAD", nil)
	if err != nil {
		return "", err
	}
	head := strings.TrimSuffix(string(out), "\n")
	if !commitID.MatchString(head) {
		return "", fmt.Errorf("git rev-parse HEAD in %s on %s printed no full commit ID", t.ProjectRoot, t.Workspace)
	}
	return head, nil
}

func (t *orcaTask) sendBrief(ctx context.Context, brief []byte) (*orcaArtifact, error) {
	created := taskRoot + "/" + t.Workspace
	script := remote.Script(
		"umask 077",
		`mkdir -p "`+taskRoot+`"`,
		`mkdir "`+created+`"`,
		`dir=$(cd "`+created+`" && pwd -P)`,
		"root=$(cd "+remote.Quote(t.ProjectRoot)+" && pwd -P)",
		`case "$dir/" in "$root"/*) exit `+fmt.Sprint(exitInsideCheckout)+` ;; esac`,
		`printf '%s\n' "$dir"`,
		`cat > "$dir/`+briefName+`"`,
		`exec cat "$dir/`+briefName+`"`,
	)
	out, err := t.fetch(ctx, "copy the brief", script, bytes.NewReader(brief))
	if err != nil {
		return nil, err
	}
	line, copied, _ := bytes.Cut(out, []byte("\n"))
	dir := string(line)
	if !path.IsAbs(dir) || path.Clean(dir) != dir || !strings.HasSuffix(dir, "/.cc-remote/orca/tasks/"+t.Workspace) {
		return nil, fmt.Errorf("the task directory on %s is not physically under $HOME/.cc-remote/orca/tasks; a linked layout is unsupported", t.Machine)
	}
	want, got := artifactOf(dir+"/"+briefName, brief), artifactOf(dir+"/"+briefName, copied)
	if got != want {
		return nil, fmt.Errorf("the copy at %s holds %d bytes with SHA-256 %s, not the brief's %d bytes with SHA-256 %s", got.Path, got.Bytes, got.SHA256, want.Bytes, want.SHA256)
	}
	return &want, nil
}

func (c orcaCollect) collect(ctx context.Context, client orca.Client, task *orcaTask) (*orcaCollection, error) {
	if !task.Prepared || task.Brief == nil {
		return nil, fmt.Errorf("%s is not a prepared task; cc-remote orca prepare makes one", task.Workspace)
	}
	dir := path.Dir(task.Brief.Path)
	for _, file := range []string{c.report, c.patch} {
		if !path.IsAbs(file) || path.Clean(file) != file || path.Dir(file) != dir || file == task.Brief.Path {
			return nil, fmt.Errorf("%q is not an artifact path in the task directory %s", file, dir)
		}
	}
	if c.report == c.patch {
		return nil, errors.New("--report-file and --patch-file name the same file")
	}
	if _, err := os.Lstat(c.output); !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("--output %s must not exist yet", c.output)
	}
	if !task.up(ctx) {
		return nil, errors.New("the forward is down; run cc-remote orca reconnect " + task.Workspace)
	}
	if _, err := client.On(task.Environment, task.RuntimeID).Status(ctx); err != nil {
		return nil, err
	}
	head, err := task.head(ctx)
	if err != nil {
		return nil, err
	}
	if head != task.BaseCommit {
		return nil, fmt.Errorf("the checkout's HEAD is %s, not the prepared baseCommit %s", head, task.BaseCommit)
	}
	report, err := task.artifact(ctx, dir, c.report)
	if err != nil {
		return nil, err
	}
	patch, err := task.artifact(ctx, dir, c.patch)
	if err != nil {
		return nil, err
	}
	if err := checkReport(report, patch, task.BaseCommit); err != nil {
		return nil, fmt.Errorf("the report at %s: %w", c.report, err)
	}
	status, err := task.status(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(c.output, 0o700); err != nil {
		return nil, err
	}
	collection := &orcaCollection{Workspace: task.Workspace, BaseCommit: task.BaseCommit, Status: status}
	if collection.Report, err = keep(c.output, c.report, report); err != nil {
		return nil, err
	}
	if collection.Patch, err = keep(c.output, c.patch, patch); err != nil {
		return nil, err
	}
	return collection, nil
}

func (t *orcaTask) artifact(ctx context.Context, dir, file string) ([]byte, error) {
	script := remote.Script(
		"dir="+remote.Quote(dir),
		"file="+remote.Quote(file),
		`test "$(cd "$dir" && pwd -P)" = "$dir" && test -f "$file" && test ! -L "$file" || exit `+fmt.Sprint(exitNotArtifact),
		`exec cat "$file"`,
	)
	return t.fetch(ctx, "read "+file, script, nil)
}

func checkReport(raw, patch []byte, base string) error {
	var report orcaReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil || report.SchemaVersion != orcaReportSchema {
		return fmt.Errorf("it is not a schema %d collection report", orcaReportSchema)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("it is not exactly one JSON document")
	}
	got := artifactOf("", patch)
	switch {
	case report.BaseCommit != base:
		return fmt.Errorf("its baseCommit is not the prepared %s", base)
	case report.Patch.SHA256 != got.SHA256 || report.Patch.Bytes != got.Bytes:
		return fmt.Errorf("it declares another digest or length than the patch's %d bytes with SHA-256 %s", got.Bytes, got.SHA256)
	}
	seen := map[string]bool{}
	for i, file := range report.Files {
		entry := file.Deleted && file.SHA256 == "" || !file.Deleted && digestHex.MatchString(file.SHA256)
		if !entry || !filepath.IsLocal(file.Path) || path.Clean(file.Path) != file.Path || seen[file.Path] {
			return fmt.Errorf("files[%d] must name a distinct repository path with either its sha256 or deleted: true", i)
		}
		seen[file.Path] = true
	}
	return nil
}

func (t *orcaTask) status(ctx context.Context) (orcaStatus, error) {
	out, err := t.fetch(ctx, "read git status", "exec git --no-optional-locks -C "+remote.Quote(t.ProjectRoot)+" status --porcelain=v1 -z --untracked-files=all", nil)
	if err != nil {
		return orcaStatus{}, err
	}
	return parseStatus(out[:min(len(out), statusLimit)], len(out)), nil
}

func parseStatus(kept []byte, total int) orcaStatus {
	status := orcaStatus{Changes: []orcaChange{}, Bytes: total, Overflow: total > len(kept)}
	fields := strings.Split(string(kept), "\x00")
	for i := 0; i < len(fields)-1; i++ {
		change := orcaChange{Status: fields[i][:2], Path: fields[i][3:]}
		if strings.ContainsAny(change.Status, "RC") {
			if i++; i == len(fields)-1 {
				break
			}
			change.From = fields[i]
		}
		status.Changes = append(status.Changes, change)
	}
	return status
}

func keep(output, source string, data []byte) (orcaArtifact, error) {
	local := filepath.Join(output, path.Base(source))
	if err := state.Write(local, data); err != nil {
		return orcaArtifact{}, err
	}
	return artifactOf(local, data), nil
}
