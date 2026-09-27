"""Addon version reported to the MCP server through get_rpc_status."""

# The freecad-mcp binary embeds this addon, so the two always ship together.
# Keep in step with <version> in ../package.xml.
__version__ = "0.2.2"

# Bump when the RPC contract changes in a way the MCP server must know about:
# a method or parameter is added or removed, or a response shape or meaning
# changes. Must match ProtocolVersion in internal/domain/domain.go.
# 2: a Placement's Rotation Angle is in degrees both ways,
#    get_active_screenshot raises a Fault on failure (None only for a view
#    that cannot be captured), and reload_document reports the name of the
#    document it reopened.
PROTOCOL_VERSION = 2
