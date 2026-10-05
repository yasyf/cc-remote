package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/remote"
)

type tarMember struct {
	name string
	mode int64
	body string
	link string
}

const stagedVersion = "#!/bin/sh\necho 0.20.0\n"

var releaseMembers = []tarMember{{name: "CHANGELOG.md", mode: 0o644, body: "notes\n"}, {name: "cc-remote", mode: 0o755, body: stagedVersion}}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func archiveOf(t *testing.T, members []tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zipped := gzip.NewWriter(&buf)
	archive := tar.NewWriter(zipped)
	for _, member := range members {
		header := &tar.Header{Name: member.name, Mode: member.mode, Size: int64(len(member.body)), Typeflag: tar.TypeReg}
		if member.link != "" {
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, member.link, 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if member.link == "" {
			if _, err := archive.Write([]byte(member.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type helperStore struct {
	dir    string
	helper *config.BootstrapHelper
}

func newHelperStore(t *testing.T, archive []byte, binary string) *helperStore {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bootstrap-helpers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := digestOf(archive)
	if err := os.WriteFile(filepath.Join(dir, sha+".tar.gz.admitted"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	return &helperStore{dir: dir, helper: &config.BootstrapHelper{Source: config.Source{SHA256: sha, Size: int64(len(archive))}, Version: "0.20.0", Platform: config.HelperPlatform, BinarySHA256: binary}}
}

func (s *helperStore) script(t *testing.T, script string) string {
	t.Helper()
	moved := strings.Replace(script, "store="+images.HelperStore+"\n", "store="+remote.Quote(s.dir)+"\n", 1)
	return strings.ReplaceAll(moved, `!= "0 755"`, `!= "`+strconv.Itoa(os.Getuid())+` 755"`)
}

func (s *helperStore) run(t *testing.T, script string, args ...string) (string, string, error) {
	t.Helper()
	return s.runWith(t, nil, script, args...)
}

func (s *helperStore) runWith(t *testing.T, env []string, script string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", s.script(t, script), "helper"}, args...)...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

func (s *helperStore) probe(t *testing.T) (string, string, error) {
	return s.run(t, helperProbeScript, s.helper.SHA256, s.helper.BinarySHA256, admissionOf(s.helper))
}

func (s *helperStore) extract(t *testing.T) (string, string, error) {
	return s.run(t, helperExtractScript, s.helper.SHA256, s.helper.BinarySHA256, strconv.FormatInt(s.helper.Size, 10))
}

func TestHelperScriptsAdmitTheFixedVerifiedMemberOnce(t *testing.T) {
	store := newHelperStore(t, archiveOf(t, releaseMembers), digestOf([]byte(stagedVersion)))
	if state, stderr, err := store.probe(t); err != nil || state != helperArchive {
		t.Fatalf("probe = %q, %v: %s", state, err, stderr)
	}
	staging, stderr, err := store.extract(t)
	if err != nil || !strings.HasPrefix(staging, store.dir+"/"+store.helper.SHA256+".") || !strings.HasSuffix(staging, ".partial") {
		t.Fatalf("extract = %q, %v: %s", staging, err, stderr)
	}
	if info, err := os.Stat(filepath.Join(staging, "cc-remote")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged binary = %v, %v", info, err)
	}
	if _, stderr, err := store.probe(t); err == nil || !strings.Contains(stderr, "is a partial bootstrap helper admission") {
		t.Errorf("a staged helper probed as %v: %s", err, stderr)
	}
	printed, err := exec.Command("sh", "-c", `exec "$1" version 2>&1`, "helper-version", filepath.Join(staging, "cc-remote")).Output()
	if err != nil || strings.TrimSpace(string(printed)) != "0.20.0" {
		t.Fatalf("staged version = %q, %v", printed, err)
	}
	if _, stderr, err := store.run(t, helperPromoteScript, store.helper.SHA256, staging, admissionOf(store.helper)); err != nil {
		t.Fatalf("promote = %v: %s", err, stderr)
	}
	if state, stderr, err := store.probe(t); err != nil || state != helperReady {
		t.Fatalf("probe after admission = %q, %v: %s", state, err, stderr)
	}
	if _, stderr, err := store.extract(t); err == nil || !strings.Contains(stderr, "an admitted bootstrap helper is never replaced") {
		t.Errorf("a second extraction = %v: %s", err, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(store.dir, store.helper.SHA256, "admission.json")); err != nil || string(got) != admissionOf(store.helper)+"\n" || strings.Contains(string(got), "url") {
		t.Errorf("admission.json = %q, %v", got, err)
	}
	binary := filepath.Join(store.dir, store.helper.SHA256, "cc-remote")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 0.21.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if state, stderr, err := store.probe(t); err == nil || state == helperReady || !strings.Contains(stderr, "it is never repaired or replaced") {
		t.Errorf("a tampered helper probed as %q, %v: %s", state, err, stderr)
	}
	if got, err := os.ReadFile(binary); err != nil || string(got) != "#!/bin/sh\necho 0.21.0\n" {
		t.Errorf("the probe changed the tampered helper: %q, %v", got, err)
	}
}

func TestHelperExtractionRefusesAnythingButTheReleasedMember(t *testing.T) {
	binary := digestOf([]byte(stagedVersion))
	tests := []struct {
		name    string
		members []tarMember
		edit    func(*helperStore)
		want    string
		partial bool
	}{
		{"another archive size", releaseMembers, func(s *helperStore) { s.helper.Size++ }, "bytes, want", false},
		{"two cc-remote members", append([]tarMember{{name: "cc-remote", mode: 0o755, body: "#!/bin/sh\necho other\n"}}, releaseMembers...), nil, "holds 2 cc-remote members", false},
		{"no cc-remote member", releaseMembers[:1], nil, "holds 0 cc-remote members", false},
		{"a nested cc-remote only", []tarMember{{name: "bin/cc-remote", mode: 0o755, body: stagedVersion}}, nil, "holds 0 cc-remote members", false},
		{"a non-executable member", []tarMember{{name: "cc-remote", mode: 0o644, body: stagedVersion}}, nil, "want a regular -rwxr-xr-x file", false},
		{"a symlink member", []tarMember{{name: "cc-remote", mode: 0o777, link: "/bin/sh"}}, nil, "want a regular -rwxr-xr-x file", false},
		{"another binary", releaseMembers, func(s *helperStore) { s.helper.BinarySHA256 = digestOf([]byte("other")) }, "member cc-remote has sha256", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newHelperStore(t, archiveOf(t, tt.members), binary)
			if tt.edit != nil {
				tt.edit(store)
			}
			staging, stderr, err := store.extract(t)
			if err == nil || !strings.Contains(stderr, tt.want) {
				t.Fatalf("extract = %q, %v: %s; want %q", staging, err, stderr, tt.want)
			}
			if _, err := os.Lstat(filepath.Join(store.dir, store.helper.SHA256)); !os.IsNotExist(err) {
				t.Errorf("a refused archive left an admitted helper: %v", err)
			}
			partials, _ := filepath.Glob(filepath.Join(store.dir, store.helper.SHA256+".*.partial"))
			if (len(partials) > 0) != tt.partial {
				t.Errorf("partial receipts = %q, want %t", partials, tt.partial)
			}
			if state, stderr, err := store.probe(t); state == helperReady || tt.partial && (err == nil || !strings.Contains(stderr, "partial bootstrap helper admission")) {
				t.Errorf("probe after the refusal = %q, %v: %s", state, err, stderr)
			}
		})
	}
	store := newHelperStore(t, archiveOf(t, releaseMembers), binary)
	if err := os.WriteFile(filepath.Join(store.dir, store.helper.SHA256+".tar.gz.admitted"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.helper.Size = int64(len("tampered"))
	if _, stderr, err := store.extract(t); err == nil || !strings.Contains(stderr, "has sha256") {
		t.Errorf("a tampered archive extracted: %v: %s", err, stderr)
	}
	empty := &helperStore{dir: t.TempDir(), helper: store.helper}
	if state, stderr, err := empty.probe(t); err != nil || state != helperAbsent {
		t.Errorf("probe of an empty store = %q, %v: %s", state, err, stderr)
	}
}

func TestHelperScriptsRefuseADigestFromAFailedOpenssl(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Fatal(err)
	}
	fakes := t.TempDir()
	fake := "#!/bin/sh\ncase \"$4\" in\n*\"$FAIL_FOR\"*) printf '%s *%s\\n' \"$MATCH\" \"$4\"; exit 7 ;;\nesac\nexec \"$REAL_OPENSSL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakes, "openssl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	failing := func(failFor, match string) []string {
		return []string{"PATH=" + fakes + ":" + os.Getenv("PATH"), "REAL_OPENSSL=" + openssl, "FAIL_FOR=" + failFor, "MATCH=" + match}
	}
	binary := digestOf([]byte(stagedVersion))

	store := newHelperStore(t, archiveOf(t, releaseMembers), binary)
	if _, stderr, err := store.runWith(t, failing(".tar.gz.admitted", store.helper.SHA256), helperExtractScript, store.helper.SHA256, binary, strconv.FormatInt(store.helper.Size, 10)); err == nil || !strings.Contains(stderr, "openssl could not hash") {
		t.Errorf("an archive digest from a failed openssl = %v: %s", err, stderr)
	}
	if partials, _ := filepath.Glob(filepath.Join(store.dir, store.helper.SHA256+".*.partial")); len(partials) != 0 {
		t.Errorf("a failed archive digest staged %q", partials)
	}

	if _, stderr, err := store.runWith(t, failing(".partial/cc-remote", binary), helperExtractScript, store.helper.SHA256, binary, strconv.FormatInt(store.helper.Size, 10)); err == nil || !strings.Contains(stderr, "openssl could not hash") {
		t.Errorf("a binary digest from a failed openssl = %v: %s", err, stderr)
	}
	if _, err := os.Lstat(filepath.Join(store.dir, store.helper.SHA256)); !os.IsNotExist(err) {
		t.Errorf("a failed binary digest admitted a helper: %v", err)
	}

	admitted := newHelperStore(t, archiveOf(t, releaseMembers), binary)
	staging, stderr, err := admitted.extract(t)
	if err != nil {
		t.Fatalf("extract = %v: %s", err, stderr)
	}
	if _, stderr, err := admitted.run(t, helperPromoteScript, admitted.helper.SHA256, staging, admissionOf(admitted.helper)); err != nil {
		t.Fatalf("promote = %v: %s", err, stderr)
	}
	state, stderr, err := admitted.runWith(t, failing(admitted.helper.SHA256+"/cc-remote", binary), helperProbeScript, admitted.helper.SHA256, binary, admissionOf(admitted.helper))
	if err == nil || state == helperReady || !strings.Contains(stderr, "openssl could not hash") {
		t.Errorf("a helper digest from a failed openssl probed as %q, %v: %s", state, err, stderr)
	}
}
