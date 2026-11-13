package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yasyf/cc-remote/internal/awskey"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
)

const envReuse = 10 * time.Minute

type heldEnv struct {
	Line     string    `json:"line"`
	Captured time.Time `json:"captured"`
}

func keyOutput(ctx context.Context, dir state.Dir, command []string) ([]byte, error) {
	if native, ok := awskey.Parse(command); ok {
		return awskey.NewStore(dir.AWSSessions(), renewBefore).Output(ctx, native)
	}
	return exec.CommandContext(ctx, command[0], command[1:]...).Output()
}

func captureKey(ctx context.Context, dir state.Dir, command []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, keyTimeout)
	defer cancel()
	out, err := keyOutput(ctx, dir, command)
	if err != nil {
		clear(out)
		return nil, fmt.Errorf("key command %s: %w", strings.Join(command, " "), err)
	}
	key := bytes.TrimSpace(out)
	if len(key) == 0 || bytes.ContainsAny(key, "\r\n") {
		clear(out)
		return nil, fmt.Errorf("key command %s must print exactly one non-empty line", strings.Join(command, " "))
	}
	return key, nil
}

func captureEnv(ctx context.Context, dir state.Dir, command []string) ([]byte, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%q", append([]string{wd}, command...)))
	path := dir.EnvLine(hex.EncodeToString(digest[:]))
	unlock, err := state.Lock(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	var held heldEnv
	found, err := state.Load(path, &held)
	if err != nil {
		return nil, err
	}
	if found && time.Since(held.Captured) < envReuse {
		return []byte(held.Line), nil
	}
	ctx, cancel := context.WithTimeout(ctx, keyTimeout)
	defer cancel()
	out, err := keyOutput(ctx, dir, command)
	defer clear(out)
	if err != nil {
		return nil, fmt.Errorf("command %s: %w", strings.Join(command, " "), err)
	}
	line, err := orca.EnvLine(out)
	if err != nil {
		return nil, fmt.Errorf("command %s %w", strings.Join(command, " "), err)
	}
	if err := state.Save(path, heldEnv{Line: string(line), Captured: time.Now()}); err != nil {
		return nil, err
	}
	return line, nil
}

func captureAWS(ctx context.Context, dir state.Dir, command []string) ([]byte, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, keyTimeout)
	defer cancel()
	out, err := keyOutput(ctx, dir, command)
	defer clear(out)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("command %s: %w", strings.Join(command, " "), err)
	}
	var session struct {
		Version         int
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Expiration      time.Time
	}
	if err := json.Unmarshal(out, &session); err != nil || session.Version != 1 || session.AccessKeyID == "" || session.SecretAccessKey == "" || session.Expiration.IsZero() {
		return nil, time.Time{}, fmt.Errorf("command %s must print the AWS process credentials, with their Expiration, that aws configure export-credentials --format process prints", strings.Join(command, " "))
	}
	var line bytes.Buffer
	if err := json.Compact(&line, out); err != nil {
		return nil, time.Time{}, err
	}
	return line.Bytes(), session.Expiration, nil
}
