# Configuration

[Back to README](../README.md) · [Installation](installation.md) · [Tools](tools.md) · [Remote access](remote-access.md)

## Environment variables

The MCP server reads its settings from environment variables, which you set in
the `env` block of the `freecad` entry in your AI client's configuration:

| Variable | Default | Purpose |
| --- | --- | --- |
| `FREECAD_MCP_HOST` | `localhost` | Host of the FreeCAD RPC server, or of a shared computer's listener; an IPv4/IPv6 address or host name. |
| `FREECAD_MCP_PORT` | `9875` for a loopback host, `9876` for any other | Port of the FreeCAD RPC server (9875) or of a shared computer's listener (9876); see [remote access](remote-access.md). |
| `FREECAD_MCP_TOKEN` | the stored password (see [remote access](remote-access.md)) | The password FreeCAD's RPC server (or a shared computer's listener) requires, when one is set. |
| `FREECAD_MCP_ONLY_TEXT_FEEDBACK` | `false` | `true` omits the optional screenshots from tool replies. |
| `FREECAD_MCP_FREECADCMD` | auto-detected | Command that starts headless FreeCAD for `execute_code_headless`. |
| `FREECAD_MCP_FREECAD` | auto-detected | Command that starts FreeCAD's GUI for `start_freecad`, when it is not found on `PATH` or in a standard install location. |
| `TRANSPORT` | `stdio` | `http` serves MCP over Streamable HTTP instead. |
| `ADDR` | `127.0.0.1:8080` | Listen address when `TRANSPORT=http`. |

An invalid value of a `FREECAD_MCP_*` variable stops the server at startup with
a message naming the variable. `TRANSPORT` serves stdio for any value other
than `http`. For example, a client entry for FreeCAD on another machine without
screenshots:

```json
{
  "mcpServers": {
    "freecad": {
      "command": "freecad-mcp",
      "args": ["mcp"],
      "env": {
        "TRANSPORT": "stdio",
        "FREECAD_MCP_HOST": "192.168.1.100",
        "FREECAD_MCP_ONLY_TEXT_FEEDBACK": "true"
      }
    }
  }
}
```

Restart your AI client after changing its configuration. Running
`freecad-mcp install` again leaves an entry you edited as it is (the wizard
shows it deselected), unless you select that client, name it with `--clients`,
or pass `--all`; `freecad-mcp uninstall` removes it like any other.

## Auto-start RPC server

A new install turns on starting the RPC server together with FreeCAD. Installing
again (`install`, `install-addon`, `update`) keeps the setting you have. The
wizard's **Start the RPC server with FreeCAD** option starts from the current
setting (on when there is none) and applies what you choose, on or off.
`freecad-mcp install-addon --no-autostart` turns it off. To change it later in
FreeCAD:

1. Switch to the **MCP Addon** workbench and open the **FreeCAD MCP** menu.
2. Check or uncheck **Auto-Start Server**.

The setting is saved to `freecad_mcp_settings.json` in FreeCAD's user data
directory and persists across sessions. With it on, the RPC server starts once
FreeCAD finishes loading.

## Starting FreeCAD from your AI client

The `start_freecad` tool starts FreeCAD's GUI on this machine when nothing
answers yet. It pings first: if FreeCAD already answers, it reports
`already_running` and starts nothing. Otherwise it launches FreeCAD detached
from the MCP server with a startup macro that starts the RPC server on the
configured port, so the addon's auto-start setting does not matter either way.
When another FreeCAD window is already open without the RPC server, that
FreeCAD receives the request instead (`forwarded`) through FreeCAD's own
single-instance handling.

`start_freecad` sets `FREECAD_MCP_PORT` in the FreeCAD process it launches, so
that if auto-start is also on, it binds the same port this server expects
instead of racing the startup macro for the default one. Set
`FREECAD_MCP_FREECAD` when FreeCAD's GUI executable is installed somewhere the
server does not find automatically; `freecad-mcp doctor` shows what it found.
After calling `start_freecad`, poll `get_rpc_status` every few seconds until
it reports `rpc: reachable`; a first start can take 15 seconds or more.

With `FREECAD_MCP_HOST` pointed at a computer sharing FreeCAD (see
[remote access](remote-access.md)), `start_freecad` works the same way but
starts FreeCAD on that computer through its listener, using the FreeCAD
command it detected there when "Share this PC" was turned on.

## Text feedback and screenshots

Set `FREECAD_MCP_ONLY_TEXT_FEEDBACK` to `true` to omit optional screenshots from
tool feedback and reduce token use.

You can also control optional screenshots per call with `include_screenshot`
and `view_name`. The environment variable takes precedence over
`include_screenshot`. See [screenshot options](tools.md#screenshot-options) for
the applicable tools and the explicit `get_view` tool.

## Remote access

By default, FreeCAD MCP only talks to FreeCAD on the same machine. To control
FreeCAD running on another computer, or to let other computers control
FreeCAD running on this one, see [remote access](remote-access.md): turning
"Share this PC" on there, the listener it starts (port 9876 by default,
alongside the addon's own loopback port 9875), the allowed IP list and
password, an SSH tunnel alternative, and the multi-agent session lock that
comes on with it.

Connect and `freecad-mcp connect` save the host and port of a shared FreeCAD
in their own store, not in these variables; `FREECAD_MCP_HOST` and
`FREECAD_MCP_PORT` (above) override what was saved when you set them
directly, and the port shown in the table is the addon's own default, not
what gets saved for a shared FreeCAD (usually 9876). Whichever way FreeCAD is
reached, [headless execution](execution.md#headless-execution) always runs on
the machine hosting the MCP server, so its file paths must be accessible
there, unlike every other tool's paths, which are on the machine running
FreeCAD.
