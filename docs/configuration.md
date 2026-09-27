# Configuration

[Back to README](../README.md) · [Installation](installation.md) · [Tools](tools.md)

## Environment variables

The MCP server reads its settings from environment variables, which you set in
the `env` block of the `freecad` entry in your AI client's configuration:

| Variable | Default | Purpose |
| --- | --- | --- |
| `FREECAD_MCP_HOST` | `localhost` | Host of the FreeCAD RPC server; an IPv4/IPv6 address or host name. |
| `FREECAD_MCP_PORT` | `9875` | Port of the FreeCAD RPC server. |
| `FREECAD_MCP_TOKEN` | the token stored with `freecad-mcp login` | Auth token the RPC server requires, when one is set in FreeCAD. |
| `FREECAD_MCP_ONLY_TEXT_FEEDBACK` | `false` | `true` omits the optional screenshots from tool replies. |
| `FREECAD_MCP_FREECADCMD` | auto-detected | Command that starts headless FreeCAD for `execute_code_headless`. |
| `TRANSPORT` | `stdio` | `http` serves MCP over Streamable HTTP instead. |
| `ADDR` | `127.0.0.1:8080` | Listen address when `TRANSPORT=http`. |

An invalid value stops the server at startup with a message naming the
variable. For example, a client entry for FreeCAD on another machine without
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

Restart your AI client after changing its configuration.

## Auto-start RPC server

The installer turns on starting the RPC server together with FreeCAD unless you
switch that off in the wizard (or pass `--no-autostart` to
`freecad-mcp install-addon`). To change it later in FreeCAD:

1. Switch to the **MCP Addon** workbench and open the **FreeCAD MCP** menu.
2. Check or uncheck **Auto-Start Server**.

The setting is saved to `freecad_mcp_settings.json` in FreeCAD's user data
directory and persists across sessions. With it on, the RPC server starts once
FreeCAD finishes loading.

## Text feedback and screenshots

Set `FREECAD_MCP_ONLY_TEXT_FEEDBACK` to `true` to omit optional screenshots from
tool feedback and reduce token use.

You can also control optional screenshots per call with `include_screenshot`
and `view_name`. The environment variable takes precedence over
`include_screenshot`. See [screenshot options](tools.md#screenshot-options) for
the applicable tools and the explicit `get_view` tool.

## Remote connections

By default, the RPC server listens on `localhost` and does not accept remote
connections. To control FreeCAD from another machine on your network, configure
both the addon and the MCP client.

Unless you [set an auth token](#3-require-an-auth-token), the RPC server has no
authentication, and it never encrypts traffic. Any program that can reach the
port from an allowed address can call every tool, including `execute_code`,
which runs arbitrary Python inside FreeCAD with your user's permissions. Set a
token whenever remote connections are on, allow only machines you trust, keep
the list as narrow as possible, and prefer an [SSH tunnel](#alternative-ssh-tunnel)
on networks you do not control. Whatever the settings, the server refuses
requests sent by web browsers, so a web page cannot call it.

### 1. Enable remote connections in FreeCAD

In the **FreeCAD MCP** toolbar:

1. Check **Remote Connections**. On the next server restart, the RPC server binds
   to `0.0.0.0` (all interfaces). It only accepts connections from the IP addresses
   or CIDR subnets configured in **Allowed IPs**, which defaults to `127.0.0.1`.
2. Click **Configure Allowed IPs** and enter a comma-separated list of allowed
   client IP addresses or CIDR subnets, for example:

   ```text
   192.168.1.100, 10.0.0.0/24
   ```

   Invalid entries are rejected with an error dialog.
3. Restart the RPC server after changing these settings.

### 2. Point the MCP server at the remote host

Set `FREECAD_MCP_HOST` to the IP address or host name of the machine running
FreeCAD, as in the [example above](#environment-variables). The value is
validated on startup.

`FREECAD_MCP_HOST` selects the GUI RPC host. [Headless execution](execution.md#headless-execution)
runs on the machine hosting the MCP server, so its file paths must be accessible
there.

### 3. Require an auth token

In the **FreeCAD MCP** toolbar, click **Set Auth Token** and enter a long random
value. Restart the RPC server. From then on, it answers only requests that carry
the token. Clear the field to turn authentication off again.

Give the MCP server the same token in one of two ways:

- `freecad-mcp login --token <the token set in FreeCAD>` stores it in
  `~/.freecad-mcp/credentials.json` (readable only by you), so it never appears
  in any AI client's configuration file. The setup wizard offers the same step.
- `FREECAD_MCP_TOKEN` in the client entry's `env` block, which takes precedence
  over the stored token.

`freecad-mcp doctor` and `freecad-mcp check-connection` report a rejected
token. The token travels unencrypted, so on a network you do not control, use
the SSH tunnel below instead.

### Alternative: SSH tunnel

To reach FreeCAD on another machine without opening the port to the network,
leave **Remote Connections** off and forward the port over SSH from the machine
that runs the MCP server:

```bash
ssh -N -L 9875:localhost:9875 user@freecad-host
```

Keep `FREECAD_MCP_HOST` at its default, `localhost`. With remote connections
off, the RPC server only answers requests addressed to `localhost` or a
loopback address such as `127.0.0.1`.
