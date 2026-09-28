"""Addon version reported to the MCP server through get_rpc_status."""

# The freecad-mcp binary embeds this addon, so the two always ship together.
# Keep in step with <version> in ../package.xml.
__version__ = "0.4.0"

# Bump when the RPC contract changes in a way the MCP server must know about:
# a method or parameter is added or removed, or a response shape or meaning
# changes. Must match ProtocolVersion in internal/domain/domain.go.
# 2: a Placement's Rotation Angle is in degrees both ways,
#    get_active_screenshot raises a Fault on failure (None only for a view
#    that cannot be captured), and reload_document reports the name of the
#    document it reopened.
# 3: document, file, recompute, printability, mesh, undo, spreadsheet,
#    measure and selection methods are added; failures carry a code and a
#    hint; get_active_screenshot takes doc_name and replies with a dict that
#    holds the image or the reason nothing was captured; get_objects takes
#    compact; object replies add bound box, centre of mass, validity and
#    linked object names; get_rpc_status reports the open documents; mutating
#    replies name the transaction that holds their changes.
# 4: the RPC server always binds 127.0.0.1 (remote access goes through the
#    freecad-mcp listener); the X-FreeCAD-MCP-Session and
#    X-FreeCAD-MCP-Client headers identify the calling session; with remote
#    access on, a session lock refuses other sessions with Fault 4230 (4231
#    once after a forced release); release_session and close_freecad are
#    added; get_rpc_status reports session; replies carry X-FreeCAD-MCP-Lock.
PROTOCOL_VERSION = 4
