package awskey

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/yasyf/cc-remote/internal/state"
)

const (
	callMargin  = 5 * time.Minute
	roleSession = time.Hour
	tokenLife   = 14 * time.Minute
	tokenMargin = 15 * time.Minute
)

type Store struct {
	dir          string
	exportMargin time.Duration
	now          func() time.Time
	load         func(ctx context.Context, command Command) (aws.Config, error)
	getParameter func(ctx context.Context, cfg aws.Config, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error)
	presign      func(ctx context.Context, cfg aws.Config, cluster string) (string, error)
}

type session struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token"`
	Source          string    `json:"source"`
	Expires         time.Time `json:"expires"`
}

type processCredentials struct {
	Version         int
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string     `json:",omitempty"`
	Expiration      *time.Time `json:",omitempty"`
}

type execCredential struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"apiVersion"`
	Spec       map[string]string `json:"spec"`
	Status     execStatus        `json:"status"`
}

type execStatus struct {
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
	Token               string    `json:"token"`
}

func NewStore(dir string, exportMargin time.Duration) *Store {
	return &Store{dir: dir, exportMargin: exportMargin, now: time.Now, load: loadConfig, getParameter: fetchParameter, presign: presignIdentity}
}

func loadConfig(ctx context.Context, command Command) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithAssumeRoleCredentialOptions(func(o *stscreds.AssumeRoleOptions) {
			if o.Duration == 0 {
				o.Duration = roleSession
			}
		}),
	}
	if command.profile != "" {
		options = append(options, config.WithSharedConfigProfile(command.profile))
	}
	if command.region != "" {
		options = append(options, config.WithRegion(command.region))
	}
	return config.LoadDefaultConfig(ctx, options...)
}

func fetchParameter(ctx context.Context, cfg aws.Config, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
	return ssm.NewFromConfig(cfg).GetParameter(ctx, input)
}

func presignIdentity(ctx context.Context, cfg aws.Config, cluster string) (string, error) {
	presigned, err := sts.NewPresignClient(sts.NewFromConfig(cfg)).PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, sts.WithAPIOptions(smithyhttp.AddHeaderValue("x-k8s-aws-id", cluster), smithyhttp.AddHeaderValue("X-Amz-Expires", "60")))
	})
	if err != nil {
		return "", err
	}
	return presigned.URL, nil
}

func (s *Store) Output(ctx context.Context, command Command) ([]byte, error) {
	cfg, err := s.load(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("load the AWS config: %w", err)
	}
	switch command.op {
	case getParameter:
		return s.parameter(ctx, cfg, command)
	case exportCredentials:
		return s.export(ctx, cfg, command)
	case getToken:
		return s.token(ctx, cfg, command)
	}
	panic(fmt.Sprintf("awskey: unknown operation %d", command.op))
}

func (s *Store) parameter(ctx context.Context, cfg aws.Config, command Command) ([]byte, error) {
	source := cfg.Credentials
	cfg.Credentials = aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		return s.credentials(ctx, source, command, callMargin)
	}))
	out, err := s.getParameter(ctx, cfg, &ssm.GetParameterInput{Name: aws.String(command.name), WithDecryption: aws.Bool(command.decrypt)})
	if err != nil {
		return nil, fmt.Errorf("ssm GetParameter %s: %w", command.name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return nil, fmt.Errorf("ssm GetParameter %s returned no value", command.name)
	}
	return append([]byte(*out.Parameter.Value), '\n'), nil
}

func (s *Store) token(ctx context.Context, cfg aws.Config, command Command) ([]byte, error) {
	creds, err := s.credentials(ctx, cfg.Credentials, command, tokenMargin)
	if err != nil {
		return nil, err
	}
	cfg.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return creds, nil })
	if cfg.Region == "" {
		cfg.Region = "aws-global"
	}
	presigned, err := s.presign(ctx, cfg, command.name)
	if err != nil {
		return nil, fmt.Errorf("presign the EKS token of %s: %w", command.name, err)
	}
	expires := s.now().UTC().Add(tokenLife)
	if creds.CanExpire && creds.Expires.Before(expires) {
		expires = creds.Expires.UTC()
	}
	out := execCredential{
		Kind:       "ExecCredential",
		APIVersion: execAPIVersion(),
		Spec:       map[string]string{},
		Status:     execStatus{ExpirationTimestamp: expires.Truncate(time.Second), Token: "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presigned))},
	}
	raw, err := json.Marshal(out) //nolint:gosec // the token is the ExecCredential kubectl reads, as aws eks get-token prints it
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func execAPIVersion() string {
	var info struct {
		APIVersion string `json:"apiVersion"`
	}
	if json.Unmarshal([]byte(os.Getenv("KUBERNETES_EXEC_INFO")), &info) == nil && info.APIVersion == "client.authentication.k8s.io/v1" {
		return info.APIVersion
	}
	return "client.authentication.k8s.io/v1beta1"
}

func (s *Store) export(ctx context.Context, cfg aws.Config, command Command) ([]byte, error) {
	creds, err := s.credentials(ctx, cfg.Credentials, command, s.exportMargin)
	if err != nil {
		return nil, err
	}
	out := processCredentials{Version: 1, AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey, SessionToken: creds.SessionToken}
	if creds.CanExpire {
		expires := creds.Expires.UTC()
		out.Expiration = &expires
	}
	raw, err := json.MarshalIndent(out, "", "    ") //nolint:gosec // printing these credentials is the command's whole output, as aws configure export-credentials prints them
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func (s *Store) credentials(ctx context.Context, source aws.CredentialsProvider, command Command, margin time.Duration) (aws.Credentials, error) {
	path := s.sessionPath(command)
	unlock, err := state.Lock(path + ".lock")
	if err != nil {
		return aws.Credentials{}, err
	}
	defer unlock()
	var held session
	found, err := state.Load(path, &held)
	if err != nil {
		return aws.Credentials{}, err
	}
	if found && held.Expires.Sub(s.now()) >= margin {
		return aws.Credentials{AccessKeyID: held.AccessKeyID, SecretAccessKey: held.SecretAccessKey, SessionToken: held.SessionToken, Source: held.Source, CanExpire: true, Expires: held.Expires}, nil
	}
	fresh, err := source.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("resolve the AWS credentials of profile %s: %w", profileName(command), redacted(err))
	}
	if !fresh.CanExpire {
		return fresh, nil
	}
	if err := state.Save(path, session{AccessKeyID: fresh.AccessKeyID, SecretAccessKey: fresh.SecretAccessKey, SessionToken: fresh.SessionToken, Source: fresh.Source, Expires: fresh.Expires}); err != nil {
		return aws.Credentials{}, err
	}
	return fresh, nil
}

func (s *Store) sessionPath(command Command) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%q", []string{os.Getenv("AWS_CONFIG_FILE"), os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), profileName(command)}))
	return filepath.Join(s.dir, hex.EncodeToString(digest[:])+".json")
}

func profileName(command Command) string {
	if command.profile != "" {
		return command.profile
	}
	for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		if profile := os.Getenv(name); profile != "" {
			return profile
		}
	}
	return "default"
}

func redacted(err error) error {
	var process *processcreds.ProviderError
	if !errors.As(err, &process) {
		return err
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return fmt.Errorf("its credential_process failed with %s", exit.ProcessState)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("its credential_process timed out")
	}
	return errors.New("its credential_process printed no usable credentials")
}
