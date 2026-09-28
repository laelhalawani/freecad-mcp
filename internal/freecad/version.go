package freecad

import (
	"fmt"

	"github.com/sairaph/freecad-mcp/internal/domain"
)

const (
	updateAddon  = "Update the addon: run `" + domain.BinaryName + " install-addon` and restart FreeCAD."
	updateServer = "Update the MCP server: run `" + domain.BinaryName + " update`."
)

// AddonVersionWarning compares the addon's get_rpc_status reply with this
// server and returns a warning, or "" when they match. A nil status means the
// addon is too old to have get_rpc_status at all.
func AddonVersionWarning(status map[string]any, serverVersion string) string {
	server := fmt.Sprintf("%s %s (protocol %d)", domain.BinaryName, serverVersion, domain.ProtocolVersion)
	if status == nil {
		return fmt.Sprintf("The FreeCAD addon is older than %s: it has no get_rpc_status. %s", server, updateAddon)
	}
	// Booleans decode as bool, never int64, so they do not count as a version.
	addonProtocol, ok := status["protocol_version"].(int64)
	if !ok {
		return fmt.Sprintf("The FreeCAD addon does not report a version, so it is older than %s. %s", server, updateAddon)
	}
	if addonProtocol == domain.ProtocolVersion {
		return ""
	}
	addonVersion := "unknown"
	if v, ok := status["addon_version"].(string); ok {
		addonVersion = v
	}
	fix := updateServer
	if addonProtocol < domain.ProtocolVersion {
		fix = updateAddon
	}
	return fmt.Sprintf("FreeCAD addon %s (protocol %d) does not match %s. %s", addonVersion, addonProtocol, server, fix)
}
