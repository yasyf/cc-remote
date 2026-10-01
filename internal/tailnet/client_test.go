package tailnet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const (
	suffix = "example.ts.net"
	tag    = "tag:cc-remote"
)

type api struct {
	t       *testing.T
	devices []Device
	keys    []map[string]any
	deleted []string
	status  int
}

func (a *api) serve() *Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "kCLIENT" || r.Form.Get("client_secret") != "tskey-client-s3cret" {
				http.Error(w, `{"message":"bad client tskey-client-echo"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"tskey-api-token","expires_in":3600}`)
		case r.Header.Get("Authorization") != "Bearer tskey-api-token":
			http.Error(w, "no bearer", http.StatusUnauthorized)
		case r.Method == http.MethodPost && r.URL.Path == "/tailnet/-/keys":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				a.t.Error(err)
			}
			a.keys = append(a.keys, body)
			_, _ = io.WriteString(w, `{"id":"kNEW","key":"tskey-auth-kNEW-minted"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/device/"):
			for _, device := range a.devices {
				if device.NodeID == strings.TrimPrefix(r.URL.Path, "/device/") {
					_ = json.NewEncoder(w).Encode(device)
					return
				}
			}
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/device/"):
			a.deleted = append(a.deleted, strings.TrimPrefix(r.URL.Path, "/device/"))
			w.WriteHeader(a.status)
		default:
			a.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	a.t.Cleanup(server.Close)
	return &Client{
		Tag:        tag,
		Base:       server.URL,
		HTTP:       server.Client(),
		Credential: Credential{ClientID: "kCLIENT", ClientSecret: "tskey-client-s3cret", Suffix: suffix},
	}
}

func mine(nodeID, hostname string) Device {
	return Device{NodeID: nodeID, Name: hostname + "." + suffix, Hostname: hostname, Tags: []string{tag}}
}

func TestMintKeyMintsASingleUseEphemeralPreauthorizedTaggedKey(t *testing.T) {
	a := &api{t: t, status: http.StatusOK}
	key, err := a.serve().MintKey(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if key != "tskey-auth-kNEW-minted" {
		t.Errorf("key = %q", key)
	}
	want := map[string]any{
		"description":   "cc-remote ws-1",
		"expirySeconds": 300.0,
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": true, "preauthorized": true, "tags": []any{tag},
		}}},
	}
	if len(a.keys) != 1 || !reflect.DeepEqual(a.keys[0], want) {
		t.Errorf("minted %v", a.keys)
	}
}

func TestErrorsCarryTheStatusAndNeverTheBodyOrSecret(t *testing.T) {
	a := &api{t: t, status: http.StatusOK}
	client := a.serve()
	client.Credential.ClientSecret = "tskey-client-wrong"
	err := client.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "echo") || strings.Contains(err.Error(), "tskey-client-wrong") {
		t.Errorf("err = %v", err)
	}
}

func TestDeleteNodeDeletesOnlyTheNodeBoundToTheWorkspace(t *testing.T) {
	a := &api{t: t, status: http.StatusOK, devices: []Device{
		mine("nMINE", "ws-1"),
		mine("nOTHER", "ws-2"),
		{NodeID: "nUNTAGGED", Name: "ws-1." + suffix, Hostname: "ws-1"},
		{NodeID: "nELSEWHERE", Name: "ws-1.other.ts.net", Hostname: "ws-1", Tags: []string{tag}},
	}}
	client := a.serve()
	if err := client.DeleteNode(context.Background(), "nMINE", "ws-1"); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"nOTHER", "nUNTAGGED", "nELSEWHERE", ""} {
		if err := client.DeleteNode(context.Background(), nodeID, "ws-1"); err == nil {
			t.Errorf("node %q of another workspace, tag or tailnet was deleted", nodeID)
		}
	}
	if err := client.DeleteNode(context.Background(), "nGONE", "ws-1"); err != nil {
		t.Errorf("an already-gone node was reported: %v", err)
	}
	if !reflect.DeepEqual(a.deleted, []string{"nMINE"}) {
		t.Errorf("deleted %v", a.deleted)
	}
}

func TestDeleteNodeReportsARefusedDeletion(t *testing.T) {
	a := &api{t: t, status: http.StatusForbidden, devices: []Device{mine("nMINE", "ws-1")}}
	if err := a.serve().DeleteNode(context.Background(), "nMINE", "ws-1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a refused delete passed: %v", err)
	}
}
