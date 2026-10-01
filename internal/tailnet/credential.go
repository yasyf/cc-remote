package tailnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	EnvClientID     = "TAILSCALE_OAUTH_CLIENT_ID"
	EnvClientSecret = "TAILSCALE_OAUTH_CLIENT_SECRET"
	EnvSuffix       = "TAILSCALE_TAILNET_SUFFIX"
	secretPrefix    = "tskey-client-"
)

type Credential struct {
	ClientID     string
	ClientSecret string
	Suffix       string
}

var (
	keychainAccount = regexp.MustCompile(`(?m)^\s*"acct"<blob>="([^"]+)"$`)
	keychainComment = regexp.MustCompile(`(?m)^\s*"icmt"<blob>="([^"]+)"$`)
	tailnetSuffix   = regexp.MustCompile(`^[a-z0-9-]+\.ts\.net$`)
)

func KeychainAdd(service string) string {
	return "security add-generic-password -a <client-id> -j <tailnet>.ts.net -s " + service + " -U -w"
}

func LoadCredential(ctx context.Context, service string) (Credential, error) {
	if credential, ok := CredentialFromEnv(os.Getenv); ok {
		return credential, nil
	}
	return CredentialFromKeychain(ctx, service)
}

func CredentialFromEnv(getenv func(string) string) (Credential, bool) {
	credential := Credential{ClientID: getenv(EnvClientID), ClientSecret: getenv(EnvClientSecret), Suffix: getenv(EnvSuffix)}
	if credential.ClientID == "" || credential.ClientSecret == "" || credential.Suffix == "" {
		return Credential{}, false
	}
	return credential, true
}

func CredentialFromKeychain(ctx context.Context, service string) (Credential, error) {
	attributes, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service).Output()
	if err != nil {
		return Credential{}, fmt.Errorf("no %s, %s and %s, and the login keychain holds no %s item; add the workspace OAuth client, its id as the account, the tailnet's DNS suffix as the comment and its secret typed at the prompt, with: %s: %w", EnvClientID, EnvClientSecret, EnvSuffix, service, KeychainAdd(service), err)
	}
	secret, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return Credential{}, fmt.Errorf("reading the %s keychain item's password: %w", service, err)
	}
	return KeychainCredential(service, attributes, secret)
}

func KeychainCredential(service string, attributes, secret []byte) (Credential, error) {
	account := keychainAccount.FindSubmatch(attributes)
	if account == nil {
		return Credential{}, fmt.Errorf("the %s keychain item names no account; its account is the OAuth client id", service)
	}
	comment := keychainComment.FindSubmatch(attributes)
	if comment == nil || !tailnetSuffix.Match(comment[1]) {
		return Credential{}, fmt.Errorf("the %s keychain item's comment is not the tailnet's <name>.ts.net DNS suffix", service)
	}
	credential := Credential{ClientID: string(account[1]), ClientSecret: strings.TrimSpace(string(secret)), Suffix: string(comment[1])}
	if !strings.HasPrefix(credential.ClientSecret, secretPrefix) {
		return Credential{}, fmt.Errorf("the %s keychain item's password is not a %s... OAuth client secret", service, secretPrefix)
	}
	return credential, nil
}
