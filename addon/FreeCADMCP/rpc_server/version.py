"""Addon version reported to the MCP server through get_rpc_status."""

# The freecad-mcp binary embeds this addon, so the two always ship together.
# Keep in step with <version> in ../package.xml.
__version__ = "0.1.25"

# Bump when the RPC contract changes in a way the MCP server must know about:
# a method or parameter is added or removed, or a response shape changes.
# Must match ProtocolVersion in internal/domain/domain.go.
PROTOCOL_VERSION = 1
