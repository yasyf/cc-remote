package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

const (
	connectionSSH    = "ssh"
	connectionServer = "server"
	connectionOrca   = "orca-server"
	orcaSchema       = 2
	orcaCheckoutMode = "provisioned-root"

	envSchemaVersion = "ORCA_RECIPE_RESULT_SCHEMA_VERSION"
	envRepoURL       = "ORCA_REPO_URL"
	envRepoRef       = "ORCA_REPO_REF"
	envRepoRefHead   = "ORCA_REPO_REF_HEAD"
	envRepoBranch    = "ORCA_REPO_BRANCH"
	envRecipeID      = "ORCA_RECIPE_ID"
	envInstanceID    = "ORCA_VM_INSTANCE_ID"
	orcaDigest       = 12
)

var unnamed = regexp.MustCompile(`[^a-z0-9]+`)

type orcaSSHTarget struct {
	Label        string        `json:"label"`
	Host         string        `json:"host"`
	Port         int           `json:"port"`
	Username     string        `json:"username"`
	IdentityFile string        `json:"identityFile,omitempty"`
	ProxyCommand string        `json:"proxyCommand,omitempty"`
	PortForwards []orcaForward `json:"portForwards,omitempty"`
}

type orcaForward struct {
	Label      string `json:"label"`
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
}

type orcaConnection struct {
	Type        string        `json:"type"`
	ProjectRoot string        `json:"projectRoot"`
	Target      orcaSSHTarget `json:"target"`
}

type orcaServerConnection struct {
	Type        string `json:"type"`
	PairingCode string `json:"pairingCode"`
	ProjectRoot string `json:"projectRoot"`
}

type orcaUserData struct {
	Provider   string `json:"provider"`
	Profile    string `json:"profile"`
	ResourceID string `json:"resourceId"`
	Machine    string `json:"machine"`
}

type orcaResult struct {
	SchemaVersion int          `json:"schemaVersion"`
	CheckoutMode  string       `json:"checkoutMode"`
	Connection    any          `json:"connection"`
	UserData      orcaUserData `json:"userData"`
}

type orcaPayload struct {
	RecipeResult struct {
		UserData orcaUserData `json:"userData"`
	} `json:"recipeResult"`
}

func orcaMode(connection string) error {
	switch connection {
	case "", connectionSSH, connectionServer:
		return nil
	}
	return fmt.Errorf("--connection %q: expected ssh or server", connection)
}

func orcaSource(env func(string) string, repository string) (workspace.Source, error) {
	if version := env(envSchemaVersion); version != fmt.Sprint(orcaSchema) {
		return workspace.Source{}, fmt.Errorf("%s=%q: cc-remote produces result schema %d (%s) only", envSchemaVersion, version, orcaSchema, orcaCheckoutMode)
	}
	if url := env(envRepoURL); strings.TrimSuffix(url, ".git") != strings.TrimSuffix(repository, ".git") {
		return workspace.Source{}, fmt.Errorf("%s=%q is not the configured repository %s", envRepoURL, url, repository)
	}
	source := workspace.Source{Ref: env(envRepoRef), Head: env(envRepoRefHead), Branch: env(envRepoBranch)}
	if source.Head == "" || source.Branch == "" {
		return workspace.Source{}, fmt.Errorf("%s and %s pin the checkout; Orca set %q and %q", envRepoRefHead, envRepoBranch, source.Head, source.Branch)
	}
	return source, source.Validate()
}

func orcaName(env func(string) string) (string, error) {
	recipe, instance := env(envRecipeID), env(envInstanceID)
	if recipe == "" || instance == "" {
		return "", fmt.Errorf("name the workspace, or let Orca set %s and %s", envRecipeID, envInstanceID)
	}
	digest := sha256.Sum256([]byte(recipe + "\x00" + instance))
	suffix := "-" + hex.EncodeToString(digest[:])[:orcaDigest]
	name := strings.Trim(unnamed.ReplaceAllString(strings.ToLower("orca-"+recipe+"-"+instance), "-"), "-")
	if len(name)+len(suffix) > state.NameLimit {
		name = strings.TrimRight(name[:state.NameLimit-len(suffix)], "-")
	}
	return name + suffix, state.ValidateName(name + suffix)
}

func orcaResource(stdin io.Reader) (string, error) {
	var payload orcaPayload
	if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
		return "", fmt.Errorf("name the workspace, or pass Orca's lifecycle payload on stdin: %w", err)
	}
	if payload.RecipeResult.UserData.ResourceID == "" {
		return "", errors.New("the lifecycle payload on stdin names no recipeResult.userData.resourceId")
	}
	return payload.RecipeResult.UserData.ResourceID, nil
}

func orcaServerResultOf(result *workspace.Result, pairing string) orcaResult {
	return orcaResult{
		SchemaVersion: orcaSchema,
		CheckoutMode:  orcaCheckoutMode,
		Connection:    orcaServerConnection{Type: connectionOrca, PairingCode: pairing, ProjectRoot: result.ProjectRoot},
		UserData:      orcaUserData{Provider: result.Provider, Profile: result.Profile, ResourceID: result.Name, Machine: result.Machine},
	}
}

func orcaResultOf(result *workspace.Result) orcaResult {
	target := orcaSSHTarget{
		Label:        result.Name,
		Host:         result.Name,
		Port:         result.SSH.Port,
		Username:     result.SSH.User,
		IdentityFile: result.SSH.IdentityFile,
		ProxyCommand: result.SSH.ProxyCommand,
	}
	for _, forward := range result.Forwards {
		target.PortForwards = append(target.PortForwards, orcaForward{Label: forward.Label, LocalPort: forward.Port, RemoteHost: "localhost", RemotePort: forward.Port})
	}
	return orcaResult{
		SchemaVersion: orcaSchema,
		CheckoutMode:  orcaCheckoutMode,
		Connection:    orcaConnection{Type: connectionSSH, ProjectRoot: result.ProjectRoot, Target: target},
		UserData:      orcaUserData{Provider: result.Provider, Profile: result.Profile, ResourceID: result.Name, Machine: result.Machine},
	}
}

func lookupEnv(name string) string {
	return os.Getenv(name)
}
