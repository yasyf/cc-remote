package awskey

import (
	"context"
	"encoding/base64"
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

func TestTokenPrintsTheExecCredentialAWSEKSGetTokenPrints(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	source := &countingSource{expires: func() time.Time { return now.Add(time.Hour) }}
	store := testStore(t, dir, &now, source)
	store.presign = func(ctx context.Context, cfg aws.Config, cluster string) (string, error) {
		if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
			return "", err
		}
		return "https://sts.us-west-2.amazonaws.com/?Action=GetCallerIdentity&cluster=" + cluster, nil
	}
	command := Command{op: getToken, region: "us-west-2", name: "frg-eks"}
	out, err := store.Output(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	var got execCredential
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Output = %q: %v", out, err)
	}
	encoded, ok := strings.CutPrefix(got.Status.Token, "k8s-aws-v1.")
	if !ok {
		t.Fatalf("token = %q, want the k8s-aws-v1. prefix", got.Status.Token)
	}
	presigned, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || string(presigned) != "https://sts.us-west-2.amazonaws.com/?Action=GetCallerIdentity&cluster=frg-eks" {
		t.Errorf("token decodes to %q, %v", presigned, err)
	}
	if got.Kind != "ExecCredential" || got.APIVersion != "client.authentication.k8s.io/v1beta1" || !got.Status.ExpirationTimestamp.Equal(now.Add(tokenLife)) {
		t.Errorf("ExecCredential = %+v", got)
	}
	now = now.Add(time.Hour - tokenMargin + time.Second)
	if _, err := store.Output(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 {
		t.Errorf("credential retrievals once the session had under the token margin left = %d, want 2", source.calls)
	}
}

func TestTokenExpiresNoLaterThanTheCredentialsThatSignedIt(t *testing.T) {
	now := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	source := &countingSource{expires: func() time.Time { return now.Add(5 * time.Minute) }}
	store := testStore(t, t.TempDir(), &now, source)
	store.presign = func(context.Context, aws.Config, string) (string, error) {
		return "https://sts.amazonaws.com/?Action=GetCallerIdentity", nil
	}
	t.Setenv("KUBERNETES_EXEC_INFO", `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential"}`)
	out, err := store.Output(t.Context(), Command{op: getToken, name: "frg-eks"})
	if err != nil {
		t.Fatal(err)
	}
	var got execCredential
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Output = %q: %v", out, err)
	}
	if !got.Status.ExpirationTimestamp.Equal(now.Add(5*time.Minute)) || got.APIVersion != "client.authentication.k8s.io/v1" {
		t.Errorf("ExecCredential = %+v; want it to expire with its credentials, on the v1 API kubectl asked for", got)
	}
}
