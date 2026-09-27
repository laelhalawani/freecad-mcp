// Package domain holds the identity and runtime settings of freecad-mcp.
package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/secret"
)

// Identity used by install, doctor and update. Owner and Repo point at the
// GitHub repository that publishes releases. ServerName is the key the AI
// clients' configs use for this server.
const (
	ServerName = "freecad"
	BinaryName = "freecad-mcp"
	Owner      = "laelhalawani"
	Repo       = "freecad-mcp"
)

// ProtocolVersion is the RPC contract version shared with the FreeCAD addon.
// It must match PROTOCOL_VERSION in addon/FreeCADMCP/rpc_server/version.py,
// which lists what each version changed.
const ProtocolVersion = 2

// DefaultRPCPort is the port the FreeCAD addon's XML-RPC server listens on.
const DefaultRPCPort = 9875

// Environment variables read by the MCP server at startup.
const (
	EnvHost             = "FREECAD_MCP_HOST"
	EnvPort             = "FREECAD_MCP_PORT"
	EnvToken            = "FREECAD_MCP_TOKEN"
	EnvOnlyTextFeedback = "FREECAD_MCP_ONLY_TEXT_FEEDBACK"
	EnvFreecadCmd       = "FREECAD_MCP_FREECADCMD"
)

// TokenKey is the credential store key holding the FreeCAD RPC auth token.
const TokenKey = "token"

// AssetName maps a GOOS/GOARCH pair to the release asset published by
// .goreleaser.yml, which names binaries <project>-<os>-<arch>[.exe].
func AssetName(goos, goarch string) string {
	name := fmt.Sprintf("%s-%s-%s", BinaryName, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// InstallDir is where install.ps1 and install.sh put the binary:
// %LOCALAPPDATA%\freecad-mcp\bin on Windows, ~/.freecad-mcp/bin elsewhere.
func InstallDir() string {
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			home, _ := os.UserHomeDir()
			local = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(local, Repo, "bin")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "."+Repo, "bin")
}

// CredentialPath returns the path where credentials are stored.
func CredentialPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("."+BinaryName, "credentials.json")
	}
	return filepath.Join(home, "."+BinaryName, "credentials.json")
}

// ProjectCredentialPath returns the credentials path inside a project directory.
// Use this with the project-scoped "add" subcommand.
func ProjectCredentialPath(dir string) string {
	return filepath.Join(dir, "."+BinaryName, "credentials.json")
}

// RemoteConfig describes a hosted MCP endpoint that this binary bridges to.
// freecad-mcp talks to a local FreeCAD, so it never bridges, and `mcp
// --remote <url>` is refused: the stored token is FreeCAD's RPC auth token.
// The type stays because main.go bridges to a configured endpoint.
type RemoteConfig struct {
	URL           string // Streamable HTTP endpoint, e.g. https://mcp.example.com/mcp
	HeaderName    string // request header carrying the credential, e.g. "Authorization"
	HeaderPrefix  string // prefix before the credential value, e.g. "Bearer "
	CredentialKey string // key in the credential store, e.g. "token"
	Prompt        string // what the login step asks for, e.g. "your API token"
}

// Remote returns nil: the binary serves its own MCP server locally.
func Remote() *RemoteConfig {
	return nil
}

// LoginConfig collects the optional auth token of the FreeCAD addon's RPC
// server. The addon accepts every local request until a token is set with
// "Set Auth Token" in FreeCAD, so the step can be skipped.
func LoginConfig(store secret.Store) installer.LoginConfig {
	return installer.LoginConfig{
		ID:        "freecad-token",
		Label:     "FreeCAD RPC auth token (optional)",
		Skippable: true,
		Stages:    loginStages(),
		Store:     store,
	}
}

func loginStages() []installer.LoginStage {
	return []installer.LoginStage{{
		Prompt: "the token set with 'Set Auth Token' in FreeCAD",
		Field: installer.LoginField{
			Name: TokenKey, Label: "Token", Masked: true,
			Validate: func(v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("the token must not be empty; to go on without one, press esc to return to Sign in / Skip for now and choose Skip for now")
				}
				return nil
			},
		},
	}}
}

// ProjectLoginConfig returns a project-scoped credential collection configuration.
// Credentials are stored via the provided store (built from ProjectCredentialPath).
func ProjectLoginConfig(store secret.Store, dir string) installer.LoginConfig {
	cfg := LoginConfig(store)
	cfg.ID = "freecad-token-project"
	cfg.Label = fmt.Sprintf("FreeCAD RPC auth token for %s (optional)", filepath.Base(dir))
	return cfg
}

// DefaultEnv returns the environment variables written into AI client
// configs when the server is registered. The server reads TRANSPORT (stdio
// or http) and ADDR (listen address for http) at startup.
func DefaultEnv() map[string]string {
	return map[string]string{
		"TRANSPORT": "stdio",
	}
}

// Settings is the MCP server's runtime configuration.
type Settings struct {
	Host             string   // FreeCAD RPC host
	Port             int      // FreeCAD RPC port
	Token            string   // FreeCAD RPC auth token; empty when the addon has none
	OnlyTextFeedback bool     // never attach screenshots
	FreecadCmd       []string // headless FreeCAD command; nil means auto-detect
}

// SettingsFromEnv reads Settings from the environment. token is the stored
// credential, used when FREECAD_MCP_TOKEN is not set.
func SettingsFromEnv(token string) (Settings, error) {
	s := Settings{Host: "localhost", Port: DefaultRPCPort, Token: strings.TrimSpace(token)}
	if v := strings.TrimSpace(os.Getenv(EnvHost)); v != "" {
		if err := ValidateHost(v); err != nil {
			return s, fmt.Errorf("%s: %w", EnvHost, err)
		}
		s.Host = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvPort)); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return s, fmt.Errorf("%s: %q is not a TCP port", EnvPort, v)
		}
		s.Port = port
	}
	if v := strings.TrimSpace(os.Getenv(EnvToken)); v != "" {
		s.Token = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvOnlyTextFeedback)); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("%s: %q is not a boolean", EnvOnlyTextFeedback, v)
		}
		s.OnlyTextFeedback = on
	}
	if v := strings.TrimSpace(os.Getenv(EnvFreecadCmd)); v != "" {
		cmd, err := SplitCommand(v)
		if err != nil {
			return s, fmt.Errorf("%s: %w", EnvFreecadCmd, err)
		}
		s.FreecadCmd = cmd
	}
	return s, nil
}
