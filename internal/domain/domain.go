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
	Owner      = "sairaph"
	Repo       = "freecad-mcp"
)

// ProtocolVersion is the RPC contract version shared with the FreeCAD addon.
// It must match PROTOCOL_VERSION in addon/FreeCADMCP/rpc_server/version.py,
// which lists what each version changed.
const ProtocolVersion = 8

// DefaultRPCPort is the port the FreeCAD addon's XML-RPC server listens on.
// The addon always binds it on 127.0.0.1.
const DefaultRPCPort = 9875

// DefaultListenerPort is the port the freecad-mcp listener (`freecad-mcp
// listen`) serves other devices on when "Share this PC" is on.
const DefaultListenerPort = 9876

// Environment variables read by the MCP server at startup.
const (
	EnvHost             = "FREECAD_MCP_HOST"
	EnvPort             = "FREECAD_MCP_PORT"
	EnvToken            = "FREECAD_MCP_TOKEN"
	EnvOnlyTextFeedback = "FREECAD_MCP_ONLY_TEXT_FEEDBACK"
	EnvFreecadCmd       = "FREECAD_MCP_FREECADCMD"
	EnvFreecadGUI       = "FREECAD_MCP_FREECAD"
	// EnvInstallCommand is set by install.ps1 and install.sh to the exact
	// command a user ran, so the wizard can show it for a re-run.
	EnvInstallCommand = "FREECAD_MCP_INSTALL_COMMAND"
)

// Keys of the one credential store (CredentialPath). The port is stored as a
// string: JSON would read a number back as float64.
const (
	TokenKey = "token" // the password of FreeCAD's RPC server (or of the listener on another computer)
	HostKey  = "host"  // FreeCAD on another computer: its host name or IP address
	PortKey  = "port"  // FreeCAD on another computer: its listener port
)

// HTTP headers shared by the MCP server, the listener and the addon.
const (
	// HeaderSession identifies the calling MCP server session (at most 128
	// characters of [A-Za-z0-9._:-]).
	HeaderSession = "X-FreeCAD-MCP-Session"
	// HeaderClient is the calling agent's label, for example "claude-code on
	// LAPTOP-2" (at most 80 printable characters).
	HeaderClient = "X-FreeCAD-MCP-Client"
	// HeaderLock is on every addon reply: "on" while the session lock is
	// enabled (remote access on), else "off".
	HeaderLock = "X-FreeCAD-MCP-Lock"
	// HeaderListener is on every listener response: ListenerMarker, or
	// ListenerFreeCADDown on the /RPC2 503 while FreeCAD is not running.
	HeaderListener = "X-FreeCAD-MCP-Listener"
)

// Values of HeaderListener.
const (
	ListenerMarker      = "1"
	ListenerFreeCADDown = "freecad-not-running"
)

// DataDir is freecad-mcp's per-user directory, ~/.freecad-mcp, which holds
// the credentials and the listener's lock and log files.
func DataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "." + BinaryName
	}
	return filepath.Join(home, "."+BinaryName)
}

// ListenerLockPath is the file lock the running listener holds.
func ListenerLockPath() string { return filepath.Join(DataDir(), "listener.lock") }

// ListenerLogPath is the listener's log file (rotated at 1 MiB, one old copy).
func ListenerLogPath() string { return filepath.Join(DataDir(), "listener.log") }

// ListenerStdoutPath is where launchd sends the listener's stdout and stderr
// on macOS. It is not ListenerLogPath: the listener rotates that file itself,
// and launchd would keep writing the rotated copy.
func ListenerStdoutPath() string { return filepath.Join(DataDir(), "listener-stdout.log") }

// wslInteropPath is a binfmt_misc entry present only under WSL (both WSL1
// and WSL2), which lets it run a Windows .exe directly.
const wslInteropPath = "/proc/sys/fs/binfmt_misc/WSLInterop"

// IsWSL reports whether this process runs inside WSL (Windows Subsystem for
// Linux): WSL sets WSL_DISTRO_NAME for every process, and wslInteropPath
// exists only there, so either alone is a reliable signal; checking both
// means one of them changing between WSL versions is not a single point of
// failure. Used to detect the same condition on both sides of a loopback
// connection (live-fixes review M1): a hostname alone is not reliable,
// since WSL takes the Windows computer's own name by default.
func IsWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	_, err := os.Stat(wslInteropPath)
	return err == nil
}

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

// CredentialPath returns the path of the one credential store every local
// agent reads: the password (TokenKey) and, for FreeCAD on another computer,
// its host and port.
func CredentialPath() string {
	return filepath.Join(DataDir(), "credentials.json")
}

// LoginConfig collects the password of FreeCAD's RPC server for `freecad-mcp
// login`. The password is set in freecad-mcp's "Share this PC" on the
// computer running FreeCAD, which stores it for the agents there too, so
// login is only needed for a manual setup.
func LoginConfig(store secret.Store) installer.LoginConfig {
	return installer.LoginConfig{
		ID:        "freecad-token",
		Label:     "FreeCAD password (optional)",
		Skippable: true,
		Stages:    loginStages(),
		Store:     store,
	}
}

func loginStages() []installer.LoginStage {
	return []installer.LoginStage{{
		Prompt: "the password set in freecad-mcp's \"Share this PC\" on the computer running FreeCAD",
		Field: installer.LoginField{
			Name: TokenKey, Label: "Password", Masked: true,
			Validate: func(v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("the password must not be empty; to go on without one, press esc to return to Sign in / Skip for now and choose Skip for now")
				}
				return nil
			},
		},
	}}
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
	Host             string   // FreeCAD host: the addon on this machine, or a listener on another
	Port             int      // the addon's RPC port, or the listener's port
	Token            string   // the password; empty when none is set
	OnlyTextFeedback bool     // never attach screenshots
	FreecadCmd       []string // headless FreeCAD command; nil means auto-detect
	FreecadGUI       []string // FreeCAD GUI command start_freecad runs; nil means auto-detect
}

// SettingsFromEnv reads Settings from the environment and the credential
// store's values (token, storedHost, storedPort; "" when not stored).
//
// Host: FREECAD_MCP_HOST, else storedHost, else localhost; "localhost" by
// any of these three is then resolved to 127.0.0.1 itself (see below). Port:
// FREECAD_MCP_PORT, else storedPort while the stored host is in effect (that
// is, FREECAD_MCP_HOST is not set), else DefaultListenerPort when the host is
// not loopback (FreeCAD on another computer is reached through its listener),
// else DefaultRPCPort. Token: FREECAD_MCP_TOKEN, else token.
func SettingsFromEnv(token, storedHost, storedPort string) (Settings, error) {
	s := Settings{Host: "localhost", Token: strings.TrimSpace(token)}
	useStoredPort := false
	if v := strings.TrimSpace(storedHost); v != "" {
		if err := ValidateHost(v); err != nil {
			return s, fmt.Errorf("stored host: %w", err)
		}
		s.Host = v
		useStoredPort = true
	}
	if v := strings.TrimSpace(os.Getenv(EnvHost)); v != "" {
		if err := ValidateHost(v); err != nil {
			return s, fmt.Errorf("%s: %w", EnvHost, err)
		}
		s.Host = v
		// The stored port belongs to the stored host: a host set in the
		// environment (for example 127.0.0.1 for FreeCAD on this machine)
		// takes its own default port instead.
		useStoredPort = false
	}
	if strings.EqualFold(strings.TrimSuffix(s.Host, "."), "localhost") {
		// "localhost." (a trailing dot, an absolute DNS name) still resolves
		// exactly like "localhost" and IsLoopbackHost already accepts it, so
		// it needs the same treatment here (live-fixes review N2). Connect
		// to 127.0.0.1 itself, never the name: "localhost" can
		// resolve to ::1 first and be answered by something else entirely
		// bound there, such as WSL's own localhost port forwarding when
		// FreeCAD is also shared from inside WSL on the same machine (live
		// check L11; see remote-access.md's WSL section). Whichever of the
		// addon or a listener actually answers 127.0.0.1 is what this
		// machine's own "localhost" is supposed to mean.
		s.Host = "127.0.0.1"
	}
	if v := strings.TrimSpace(storedPort); v != "" && useStoredPort {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return s, fmt.Errorf("stored port %q is not a TCP port", v)
		}
		s.Port = port
	}
	if v := strings.TrimSpace(os.Getenv(EnvPort)); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return s, fmt.Errorf("%s: %q is not a TCP port", EnvPort, v)
		}
		s.Port = port
	}
	if s.Port == 0 {
		s.Port = DefaultRPCPort
		if !IsLoopbackHost(s.Host) {
			s.Port = DefaultListenerPort
		}
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
	if v := strings.TrimSpace(os.Getenv(EnvFreecadGUI)); v != "" {
		cmd, err := SplitCommand(v)
		if err != nil {
			return s, fmt.Errorf("%s: %w", EnvFreecadGUI, err)
		}
		s.FreecadGUI = cmd
	}
	return s, nil
}
