package addoninstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Keys of the addon settings file (rpc_server/settings.py holds the same
// defaults).
const (
	KeyRemoteEnabled  = "remote_enabled"          // bool, default false
	KeyAllowedIPs     = "allowed_ips"             // string, default DefaultAllowedIPs; never empty
	KeyAutoStart      = "auto_start_rpc"          // bool, default false
	KeyAuthToken      = "auth_token"              // string, default "" (no password)
	KeySessionTimeout = "session_timeout_minutes" // int, 1 to 1440, default 30
	KeyListenerPort   = "listener_port"           // int, 1 to 65535, default 9876
)

// Defaults and ranges of the remote access settings.
const (
	DefaultAllowedIPs            = "127.0.0.1"
	DefaultSessionTimeoutMinutes = 30
	MinSessionTimeoutMinutes     = 1
	MaxSessionTimeoutMinutes     = 1440
	DefaultListenerPort          = 9876
)

// RemoteSettings are the remote access settings of one FreeCAD user data
// directory, with defaults for keys that are missing or out of range.
type RemoteSettings struct {
	RemoteEnabled         bool
	AllowedIPs            string
	AuthToken             string
	SessionTimeoutMinutes int
	ListenerPort          int
}

// DefaultRemoteSettings are the settings of a file without remote keys.
func DefaultRemoteSettings() RemoteSettings {
	return RemoteSettings{
		AllowedIPs:            DefaultAllowedIPs,
		SessionTimeoutMinutes: DefaultSessionTimeoutMinutes,
		ListenerPort:          DefaultListenerPort,
	}
}

// SettingsPath is the addon settings file in t.
func SettingsPath(t Target) string { return filepath.Join(t.UserDataDir, SettingsFile) }

// ReadRemoteSettings reads the remote access settings of t. A missing file
// gives the defaults; a file that is not a JSON object is an error. A key
// that is missing, of the wrong type or out of range takes its default (an
// allowed IP list that is empty or blank too), except auth_token: present
// with any type but string is an error (never silently "no password"), the
// same fail-safe as an allowed list that cannot be parsed; the list itself
// is not validated here (domain.ParseAllowedIPs does that where it is used).
func ReadRemoteSettings(t Target) (RemoteSettings, error) {
	s := DefaultRemoteSettings()
	values, err := readSettings(SettingsPath(t))
	if err != nil {
		return s, err
	}
	if v, ok := values[KeyRemoteEnabled].(bool); ok {
		s.RemoteEnabled = v
	}
	if v, ok := values[KeyAllowedIPs].(string); ok && strings.TrimSpace(v) != "" {
		s.AllowedIPs = v
	}
	if raw, present := values[KeyAuthToken]; present {
		v, ok := raw.(string)
		if !ok {
			return DefaultRemoteSettings(), fmt.Errorf("%s must be a string", KeyAuthToken)
		}
		s.AuthToken = v
	}
	if v, ok := wholeNumber(values[KeySessionTimeout]); ok && v >= MinSessionTimeoutMinutes && v <= MaxSessionTimeoutMinutes {
		s.SessionTimeoutMinutes = v
	}
	if v, ok := wholeNumber(values[KeyListenerPort]); ok && v >= 1 && v <= 65535 {
		s.ListenerPort = v
	}
	return s, nil
}

// UpdateSettings writes values into t's settings file, keeping every other
// key (read, merge, write). A nil value removes its key. The file is replaced
// atomically and is readable by its owner only (0600): it holds the
// password.
func UpdateSettings(t Target, values map[string]any) error {
	file := SettingsPath(t)
	settings, err := readSettings(file)
	if err != nil {
		return err
	}
	for k, v := range values {
		if v == nil {
			delete(settings, k)
			continue
		}
		settings[k] = v
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.UserDataDir, 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp-" + suffix()
	if err := writeSynced(tmp, out, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readSettings returns the settings file's object, an empty map when the
// file is missing or holds JSON null, or an error when it cannot be read or
// is not a JSON object.
func readSettings(file string) (map[string]any, error) {
	settings := map[string]any{}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON; fix or delete it: %w", file, err)
	}
	if settings == nil { // the file held JSON null
		settings = map[string]any{}
	}
	return settings, nil
}

// wholeNumber reads a JSON number (float64) that holds an integer.
func wholeNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}
