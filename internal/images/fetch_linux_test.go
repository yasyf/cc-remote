package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFetchPayloadAdmitsOnlyTheExactBytes(t *testing.T) {
	payload, wrong := []byte("hsqs the exact payload bytes"), []byte("hsqs the wrong payload bytes")
	sum, wrongSum := sha256.Sum256(payload), sha256.Sum256(wrong)
	sha, wrongSha := hex.EncodeToString(sum[:]), hex.EncodeToString(wrongSum[:])
	size, half := strconv.Itoa(len(payload)), len(payload)/2
	const sentinel = "X-Amz-Signature=0123456789abcdef0123456789abcdef"
	tests := []struct {
		name    string
		handler http.HandlerFunc
		plain   bool
		wantErr string
	}{
		{name: "the exact bytes are admitted", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }},
		{name: "an expired url is refused with a hint", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "AccessDenied", http.StatusForbidden)
		}, wantErr: "cc-remote: the payload download failed (curl exit 22, HTTP 403, tee exit 0, sha256sum exit 0); a 403 usually means the presigned URL expired"},
		{name: "a missing object is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
		}, wantErr: "cc-remote: the payload download failed (curl exit 22, HTTP 404, tee exit 0, sha256sum exit 0)"},
		{name: "a corrupt body is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(wrong)
		}, wantErr: "cc-remote: the payload has sha256 " + wrongSha + ", want " + sha},
		{name: "a truncated body is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", size)
			_, _ = w.Write(payload[:half])
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(err)
			}
			_ = conn.Close()
		}, wantErr: "cc-remote: the payload download failed (curl exit 18,"},
		{name: "a short body of unknown length is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload[:half])
			w.(http.Flusher).Flush()
		}, wantErr: "cc-remote: the payload download is " + strconv.Itoa(half) + " bytes, want " + size},
		{name: "an oversize body is refused before it downloads", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)+1))
			_, _ = w.Write(slices.Concat(payload, []byte{'!'}))
		}, wantErr: "cc-remote: the payload download failed (curl exit 63,"},
		{name: "a redirect is not followed", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
		}, wantErr: "cc-remote: the payload URL answered HTTP 302, want 200"},
		{name: "a plain http url is refused before it connects", plain: true, wantErr: "cc-remote: the payload download failed (curl exit 1,"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			store := filepath.Join(root, "store")
			stale := stageStaleDownload(t, store, sha)
			target, env := "http://127.0.0.1:1/tools.sqfs?"+sentinel, os.Environ()
			if !tt.plain {
				server := httptest.NewTLSServer(tt.handler)
				t.Cleanup(server.Close)
				bundle := filepath.Join(root, "ca.pem")
				if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
					t.Fatal(err)
				}
				target, env = server.URL+"/tools.sqfs?"+sentinel, append(env, "CURL_CA_BUNDLE="+bundle)
			}
			fetch := strings.Replace(fetchPayload, "store="+PayloadStore+"\n", "store="+quote(store)+"\n", 1)
			cmd := exec.Command("bash", "-c", fetch, "fetch-payload", sha, size)
			cmd.Stdin = bytes.NewReader(PayloadURL{raw: target}.curlConfig())
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			for _, text := range slices.Concat([]string{string(out)}, cmd.Args, cmd.Env) {
				if strings.Contains(text, sentinel) {
					t.Errorf("%q carries the signed query", text)
				}
			}
			if strings.Contains(string(out), "127.0.0.1") {
				t.Errorf("the output names the host:\n%s", out)
			}
			admitted := filepath.Join(store, sha+".sqfs.admitted")
			assertOnlyTheStaleDownloadRemains(t, store, stale)
			if tt.wantErr != "" {
				if exitCode(err) != 1 || !strings.Contains(string(out), tt.wantErr) {
					t.Fatalf("fetch = %v\n%s\nwant exit 1 with %q", err, out, tt.wantErr)
				}
				if _, err := os.Stat(admitted); !os.IsNotExist(err) {
					t.Errorf("a failed download was admitted: %v", err)
				}
				return
			}
			if err != nil || len(out) != 0 {
				t.Fatalf("fetch = %v\n%s\nwant a silent success", err, out)
			}
			if got, err := os.ReadFile(admitted); err != nil || !bytes.Equal(got, payload) {
				t.Errorf("admitted %q, %v; want the payload", got, err)
			}
		})
	}
}
