package sprites

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	apiBase         = "https://api.sprites.dev"
	tokenEnv        = "SPRITE_TOKEN"
	keychain        = "/usr/bin/security"
	keychainMissing = 44
	keyringBase64   = "go-keyring-base64:"
)

var credentials sync.Map

func credential(org string) func() string {
	resolve, _ := credentials.LoadOrStore(org, sync.OnceValue(func() string {
		token, err := resolveToken(org)
		if err != nil {
			slog.Warn("sprites: reading the API token failed; the sprite CLI reads its own", "org", org, "err", err)
		}
		return token
	}))
	return resolve.(func() string)
}

func resolveToken(org string) (string, error) {
	if token := os.Getenv(tokenEnv); token != "" {
		return token, nil
	}
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".sprites", "sprites.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		CurrentUser string `json:"current_user"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("reading the sprite CLI config: %w", err)
	}
	if config.CurrentUser == "" {
		return "", nil
	}
	out, err := exec.Command(keychain, "find-generic-password", "-s", "sprites-cli:"+config.CurrentUser, "-a", "sprites:org:"+apiBase+":"+org, "-w").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == keychainMissing {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the sprites token from the keychain: %w", err)
	}
	return decodeKeyring(strings.TrimSpace(string(out)))
}

func decodeKeyring(stored string) (string, error) {
	encoded, ok := strings.CutPrefix(stored, keyringBase64)
	if !ok {
		return stored, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding the keychain's sprites token: %w", err)
	}
	return string(decoded), nil
}
