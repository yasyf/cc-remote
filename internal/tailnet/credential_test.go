package tailnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const service = "cc-remote-tailnet"

const keychainItem = `keychain: "/Users/o/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    0x00000007 <blob>="cc-remote-tailnet"
    "acct"<blob>="kCHAIN"
    "icmt"<blob>="example.ts.net"
    "svce"<blob>="cc-remote-tailnet"
`

func TestKeychainCredentialReadsTheClientIdAndTailnetFromTheItem(t *testing.T) {
	got, err := KeychainCredential(service, []byte(keychainItem), []byte("tskey-client-chain\n"))
	if err != nil || got != (Credential{ClientID: "kCHAIN", ClientSecret: "tskey-client-chain", Suffix: suffix}) {
		t.Errorf("credential = %+v, err = %v", got, err)
	}
	if _, err := KeychainCredential(service, []byte(strings.Replace(keychainItem, `"acct"<blob>="kCHAIN"`, "", 1)), []byte("tskey-client-chain")); err == nil {
		t.Error("an item with no account was accepted")
	}
	if _, err := KeychainCredential(service, []byte(strings.Replace(keychainItem, "example.ts.net", "example.com", 1)), []byte("tskey-client-chain")); err == nil {
		t.Error("an item whose comment is not a ts.net suffix was accepted")
	}
	if _, err := KeychainCredential(service, []byte(keychainItem), []byte("hunter2")); err == nil {
		t.Error("a password that is not an OAuth client secret was accepted")
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvClientID, EnvClientSecret, EnvSuffix} {
		t.Setenv(name, "")
	}
}

func TestLoadCredentialPrefersTheEnvironmentThenTheKeychain(t *testing.T) {
	bin := t.TempDir()
	item := filepath.Join(t.TempDir(), "item")
	if err := os.WriteFile(item, []byte(keychainItem), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBinary(t, bin, "security", `[ "$1 $2 $3" = "find-generic-password -s `+service+`" ] || exit 44
if [ "$4" = -w ]; then echo tskey-client-chain; else cat "`+item+`"; fi`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	clearEnv(t)
	got, err := LoadCredential(context.Background(), service)
	if err != nil || got != (Credential{ClientID: "kCHAIN", ClientSecret: "tskey-client-chain", Suffix: suffix}) {
		t.Errorf("keychain credential = %+v, err = %v", got, err)
	}
	t.Setenv(EnvClientID, "kENV")
	t.Setenv(EnvClientSecret, "tskey-client-env")
	t.Setenv(EnvSuffix, "env.ts.net")
	if got, err := LoadCredential(context.Background(), service); err != nil || got != (Credential{ClientID: "kENV", ClientSecret: "tskey-client-env", Suffix: "env.ts.net"}) {
		t.Errorf("environment credential = %+v, err = %v", got, err)
	}
	t.Setenv(EnvSuffix, "")
	if got, err := LoadCredential(context.Background(), service); err != nil || got.ClientID != "kCHAIN" {
		t.Errorf("a partial environment did not fall back to the keychain: %+v, %v", got, err)
	}
}

func TestLoadCredentialNamesTheKeychainItemToAdd(t *testing.T) {
	bin := t.TempDir()
	fakeBinary(t, bin, "security", "exit 44")
	t.Setenv("PATH", bin)
	clearEnv(t)
	_, err := LoadCredential(context.Background(), "other-service")
	if err == nil || !strings.Contains(err.Error(), KeychainAdd("other-service")) || strings.Contains(err.Error(), "-w '") {
		t.Errorf("err = %v", err)
	}
}
