package awskey

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type countingSource struct {
	calls   int
	expires func() time.Time
}

func (c *countingSource) Retrieve(context.Context) (aws.Credentials, error) {
	c.calls++
	creds := aws.Credentials{AccessKeyID: "ASIA-sentinel", SecretAccessKey: "secret-sentinel", SessionToken: "token-sentinel", Source: "test"}
	if c.expires != nil {
		creds.CanExpire, creds.Expires = true, c.expires()
	}
	return creds, nil
}

func testStore(t *testing.T, dir string, now *time.Time, source aws.CredentialsProvider) *Store {
	t.Helper()
	return &Store{
		dir:          dir,
		exportMargin: time.Hour,
		now:          func() time.Time { return *now },
		load: func(context.Context, Command) (aws.Config, error) {
			return aws.Config{Credentials: source}, nil
		},
		getParameter: func(ctx context.Context, cfg aws.Config, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
				return nil, err
			}
			if !*input.WithDecryption {
				t.Errorf("GetParameter WithDecryption = false")
			}
			return &ssm.GetParameterOutput{Parameter: &types.Parameter{Name: input.Name, Value: aws.String("value-sentinel"), Version: 3}}, nil
		},
	}
}

func TestParameterReusesTheProfilesSessionAcrossStoresAndNeverStoresTheValue(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	source := &countingSource{expires: func() time.Time { return now.Add(time.Hour) }}
	command := Command{op: getParameter, profile: "ops", region: "us-west-2", name: "/ci/key", decrypt: true}
	for range 2 {
		out, err := testStore(t, dir, &now, source).Output(t.Context(), command)
		if err != nil || string(out) != "value-sentinel\n" {
			t.Fatalf("Output = %q, %v", out, err)
		}
	}
	if source.calls != 1 {
		t.Errorf("credential retrievals = %d, want 1", source.calls)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("session files = %v, %v", files, err)
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || strings.Contains(string(raw), "value-sentinel") {
		t.Errorf("the session file holds the parameter value: %s, %v", raw, err)
	}
	now = now.Add(time.Hour - callMargin + time.Second)
	if _, err := testStore(t, dir, &now, source).Output(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 {
		t.Errorf("credential retrievals after the session neared its end = %d, want 2", source.calls)
	}
}

func TestExportPrintsProcessCredentialsAndRenewsASessionWithUnderTheMarginLeft(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	source := &countingSource{expires: func() time.Time { return now.Add(4 * time.Hour) }}
	command := Command{op: exportCredentials, profile: "lane"}
	out, err := testStore(t, dir, &now, source).Output(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	var got processCredentials
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Output = %q: %v", out, err)
	}
	want := now.Add(4 * time.Hour)
	if got.Version != 1 || got.AccessKeyID != "ASIA-sentinel" || got.SecretAccessKey != "secret-sentinel" || got.SessionToken != "token-sentinel" || got.Expiration == nil || !got.Expiration.Equal(want) {
		t.Errorf("Output = %+v", got)
	}
	now = now.Add(2 * time.Hour)
	if _, err := testStore(t, dir, &now, source).Output(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Errorf("credential retrievals with two hours left = %d, want 1", source.calls)
	}
	now = now.Add(time.Hour + time.Second)
	if _, err := testStore(t, dir, &now, source).Output(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 {
		t.Errorf("credential retrievals with under an hour left = %d, want 2", source.calls)
	}
}

func TestStaticCredentialsAreNeverWritten(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	source := &countingSource{}
	out, err := testStore(t, dir, &now, source).Output(t.Context(), Command{op: exportCredentials, profile: "static"})
	if err != nil || strings.Contains(string(out), "Expiration") {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if files, err := filepath.Glob(filepath.Join(dir, "*.json")); err != nil || len(files) != 0 {
		t.Errorf("session files = %v, %v; want none", files, err)
	}
}
