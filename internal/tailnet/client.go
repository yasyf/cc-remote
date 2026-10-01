package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/yasyf/cc-remote/internal/remote"
)

const (
	keyExpirySeconds = 300
	maxBody          = 1 << 20
)

var ErrNoNode = errors.New("the tailnet holds no such node")

type Client struct {
	Tag        string
	Base       string
	Credential Credential
	HTTP       *http.Client
}

type Device struct {
	NodeID   string   `json:"nodeId"`
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
}

func (c *Client) token(ctx context.Context) (string, error) {
	form := url.Values{"client_id": {c.Credential.ClientID}, "client_secret": {c.Credential.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	operation := "exchanging the tailnet OAuth client " + c.Credential.ClientID + " for a token"
	if err := c.call(req, operation, &token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("%s returned no access_token", operation)
	}
	return token.AccessToken, nil
}

func readBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return body, errors.Join(err, resp.Body.Close())
}

func (c *Client) call(req *http.Request, operation string, into any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	body, err := readBody(resp)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", operation, resp.Status)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s answered %s with a body that is not the expected JSON", operation, resp.Status)
	}
	return nil
}

func (c *Client) authorized(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) Verify(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}

func (c *Client) MintKey(ctx context.Context, hostname string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"description":   remote.Prefix + " " + hostname,
		"expirySeconds": keyExpirySeconds,
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable":      false,
			"ephemeral":     true,
			"preauthorized": true,
			"tags":          []string{c.Tag},
		}}},
	})
	if err != nil {
		return "", err
	}
	req, err := c.authorized(ctx, http.MethodPost, "/tailnet/-/keys", body)
	if err != nil {
		return "", err
	}
	var key struct {
		Key string `json:"key"`
	}
	operation := "minting a " + c.Tag + " auth key for " + hostname
	if err := c.call(req, operation, &key); err != nil {
		return "", err
	}
	if key.Key == "" {
		return "", fmt.Errorf("%s returned no key", operation)
	}
	return key.Key, nil
}

func (c *Client) Device(ctx context.Context, nodeID string) (Device, error) {
	req, err := c.authorized(ctx, http.MethodGet, "/device/"+url.PathEscape(nodeID), nil)
	if err != nil {
		return Device{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Device{}, fmt.Errorf("reading tailnet node %s: %w", nodeID, err)
	}
	body, err := readBody(resp)
	if err != nil {
		return Device{}, fmt.Errorf("reading tailnet node %s: %w", nodeID, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Device{}, ErrNoNode
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Device{}, fmt.Errorf("reading tailnet node %s answered %s", nodeID, resp.Status)
	}
	var device Device
	if err := json.Unmarshal(body, &device); err != nil || device.NodeID == "" {
		return Device{}, fmt.Errorf("reading tailnet node %s answered %s with a body that is not a device", nodeID, resp.Status)
	}
	return device, nil
}

func (c *Client) owns(device Device, hostname string) bool {
	return device.Hostname == hostname && slices.Contains(device.Tags, c.Tag) && strings.HasSuffix(device.Name, "."+c.Credential.Suffix)
}

func (c *Client) Owns(ctx context.Context, nodeID, hostname string) error {
	if nodeID == "" {
		return errors.New("no tailnet node id was recorded")
	}
	device, err := c.Device(ctx, nodeID)
	if err != nil {
		return err
	}
	if !c.owns(device, hostname) {
		return fmt.Errorf("tailnet node %s is %s with tags %v, not the %s node of workspace %s", nodeID, device.Name, device.Tags, c.Tag, hostname)
	}
	return nil
}

func (c *Client) DeleteNode(ctx context.Context, nodeID, hostname string) error {
	err := c.Owns(ctx, nodeID, hostname)
	if errors.Is(err, ErrNoNode) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing to delete: %w", err)
	}
	req, err := c.authorized(ctx, http.MethodDelete, "/device/"+url.PathEscape(nodeID), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("deleting tailnet node %s: %w", nodeID, err)
	}
	if _, err := readBody(resp); err != nil {
		return fmt.Errorf("deleting tailnet node %s: %w", nodeID, err)
	}
	if resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return fmt.Errorf("deleting tailnet node %s answered %s", nodeID, resp.Status)
	}
	return nil
}
