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
	"sync"
	"testing"
	"time"
)

func serveRanges(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}
}

func TestFetchAdmitsOnlyTheExactBytes(t *testing.T) {
	payload, wrong := []byte("hsqs\x00the exact payload bytes\n"), []byte("hsqs\x00the wrong payload bytes\n")
	sum, wrongSum := sha256.Sum256(payload), sha256.Sum256(wrong)
	sha, wrongSha := hex.EncodeToString(sum[:]), hex.EncodeToString(wrongSum[:])
	size, half := strconv.Itoa(len(payload)), len(payload)/2
	const sentinel = "X-Amz-Signature=0123456789abcdef0123456789abcdef"
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		plain     bool
		hashFails bool
		wantErr   string
	}{
		{name: "the exact bytes are admitted", handler: serveRanges(payload)},
		{name: "an expired url is refused with a hint", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "AccessDenied", http.StatusForbidden)
		}, wantErr: " download failed (curl exit 22, HTTP 403, tee exit 0, openssl exit 0); a 403 usually means the presigned URL expired"},
		{name: "a missing object is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
		}, wantErr: " download failed (curl exit 22, HTTP 404, tee exit 0, openssl exit 0)"},
		{name: "a corrupt body is refused", handler: serveRanges(wrong), wantErr: " has sha256 " + wrongSha + ", want " + sha},
		{name: "a truncated body is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", size)
			_, _ = w.Write(payload[:half])
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(err)
			}
			_ = conn.Close()
		}, wantErr: " download failed (curl exit 18,"},
		{name: "a short body of unknown length is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:half])
			w.(http.Flusher).Flush()
		}, wantErr: " download is " + strconv.Itoa(half) + " bytes, want " + size},
		{name: "an oversize body is refused before it downloads", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)+1))
			_, _ = w.Write(slices.Concat(payload, []byte{'!'}))
		}, wantErr: " download failed (curl exit 63,"},
		{name: "a redirect is not followed", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
		}, wantErr: " URL answered HTTP 302, want 206"},
		{name: "a server that ignores the range is refused", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		}, wantErr: " URL answered HTTP 200, want 206"},
		{name: "a plain http url is refused before it connects", plain: true, wantErr: " download failed (curl exit 1,"},
		{name: "a failed digest cannot admit its matching output", handler: serveRanges(payload), hashFails: true, wantErr: " download failed (curl exit 0, HTTP 206, tee exit 0, openssl exit 7)"},
	}
	for _, artifact := range append(slices.Clone(stagedArtifacts), HelperArtifact) {
		for _, tt := range tests {
			t.Run(artifact.Label+"/"+tt.name, func(t *testing.T) {
				root := t.TempDir()
				store := filepath.Join(root, "store")
				stale := stageStaleDownload(t, store, sha, artifact)
				target, env := "http://127.0.0.1:1/tools"+artifact.Suffix+"?"+sentinel, os.Environ()
				if !tt.plain {
					server := httptest.NewTLSServer(tt.handler)
					t.Cleanup(server.Close)
					bundle := filepath.Join(root, "ca.pem")
					if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
						t.Fatal(err)
					}
					target, env = server.URL+"/tools"+artifact.Suffix+"?"+sentinel, append(env, "CURL_CA_BUNDLE="+bundle)
				}
				fetch := strings.Replace(artifact.fetchScript(), "store="+artifact.Store+"\n", "store="+quote(store)+"\n", 1)
				cmd := exec.Command("bash", "-c", fetch, "fetch-"+artifact.Label, sha, size)
				cmd.Stdin = bytes.NewReader(PayloadURL{raw: target}.curlConfig())
				cmd.Env = env
				if tt.hashFails {
					fakes := t.TempDir()
					writeFakes(t, fakes, map[string]string{"openssl": "#!/bin/sh\ncat > /dev/null\nprintf '%s  -\\n' " + sha + "\nexit 7\n"})
					cmd.Env = append(cmd.Env, "PATH="+fakes+":"+os.Getenv("PATH"))
				}
				out, err := cmd.CombinedOutput()
				for _, text := range slices.Concat([]string{string(out)}, cmd.Args, cmd.Env) {
					if strings.Contains(text, sentinel) {
						t.Errorf("%q carries the signed query", text)
					}
				}
				if strings.Contains(string(out), "127.0.0.1") {
					t.Errorf("the output names the host:\n%s", out)
				}
				admitted := filepath.Join(store, sha+artifact.Suffix+".admitted")
				assertOnlyTheStaleDownloadRemains(t, store, stale)
				if tt.wantErr != "" {
					if want := "cc-remote: the " + artifact.Name + tt.wantErr; exitCode(err) != 1 || !strings.Contains(string(out), want) {
						t.Fatalf("fetch = %v\n%s\nwant exit 1 with %q", err, out, want)
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
					t.Errorf("admitted %q, %v; want the %s", got, err, artifact.Name)
				}
			})
		}
	}
}

func TestFetchAdmitsRangesInOrder(t *testing.T) {
	const size, span = 1000, 37
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*7 + i/13)
	}
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	refused := "bytes=" + strconv.Itoa(5*span) + "-" + strconv.Itoa(6*span-1)
	tests := []struct {
		name    string
		slow    string
		refuse  string
		wantErr string
	}{
		{name: "every range is admitted in order"},
		{name: "a first range that arrives last is still admitted first", slow: "bytes=0-" + strconv.Itoa(span-1)},
		{name: "one refused range refuses the download", refuse: refused, wantErr: " download failed (curl exit 22, HTTP 403, tee exit 0, openssl exit 0); a 403 usually means the presigned URL expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			open, most := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asks := r.Header.Get("Range")
				mu.Lock()
				asked = append(asked, asks)
				open++
				most = max(most, open)
				mu.Unlock()
				defer func() {
					mu.Lock()
					open--
					mu.Unlock()
				}()
				switch asks {
				case "":
					t.Errorf("a request carried no range")
				case tt.refuse:
					http.Error(w, "AccessDenied", http.StatusForbidden)
					return
				case tt.slow:
					time.Sleep(300 * time.Millisecond)
				}
				serveRanges(payload)(w, r)
			}))
			t.Cleanup(server.Close)
			root := t.TempDir()
			store, bundle := filepath.Join(root, "store"), filepath.Join(root, "ca.pem")
			if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			stale := stageStaleDownload(t, store, sha, PayloadArtifact)
			fetch := strings.Replace(PayloadArtifact.fetchScript(), "store="+PayloadStore+"\n", "store="+quote(store)+"\n", 1)
			fetch = strings.Replace(fetch, "range="+strconv.Itoa(fetchRange)+"\n", "range="+strconv.Itoa(span)+"\n", 1)
			cmd := exec.Command("bash", "-c", fetch, "fetch-payload", sha, strconv.Itoa(size))
			cmd.Stdin = bytes.NewReader(PayloadURL{raw: server.URL + "/tools.sqfs"}.curlConfig())
			cmd.Env = append(os.Environ(), "CURL_CA_BUNDLE="+bundle)
			out, err := cmd.CombinedOutput()
			if most > fetchStreams {
				t.Errorf("%d ranges were open at once, want at most %d", most, fetchStreams)
			}
			admitted := filepath.Join(store, sha+".sqfs.admitted")
			assertOnlyTheStaleDownloadRemains(t, store, stale)
			if tt.wantErr != "" {
				if want := "cc-remote: the payload" + tt.wantErr; exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("fetch = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				if _, err := os.Stat(admitted); !os.IsNotExist(err) {
					t.Errorf("a refused download was admitted: %v", err)
				}
				return
			}
			if err != nil || len(out) != 0 {
				t.Fatalf("fetch = %v\n%s\nwant a silent success", err, out)
			}
			if got, err := os.ReadFile(admitted); err != nil || !bytes.Equal(got, payload) {
				t.Errorf("admitted %d bytes, %v; want the %d payload bytes in order", len(got), err, size)
			}
			var want []string
			for start := 0; start < size; start += span {
				want = append(want, "bytes="+strconv.Itoa(start)+"-"+strconv.Itoa(min(start+span, size)-1))
			}
			slices.Sort(asked)
			slices.Sort(want)
			if !slices.Equal(asked, want) {
				t.Errorf("the server was asked for %q, want each range once: %q", asked, want)
			}
		})
	}
}
