# Installation

[Back to README](../README.md) · [Configuration](configuration.md)

FreeCAD MCP has two parts:

- the **MCP server**, a single self-contained `freecad-mcp` binary that your AI
  client starts. It has no runtime dependencies: no Python, uv or pip.
- the **FreeCAD addon**, which runs inside FreeCAD on FreeCAD's own bundled
  Python and serves the XML-RPC API the server calls. The binary embeds the
  addon and installs it, so the two always match.

## One-line install

Install [FreeCAD](https://www.freecad.org/downloads.php) first, then run:

Windows (PowerShell):

```powershell
irm https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.sh | sh
```

The script downloads the binary for your platform, verifies it against the
release's `SHA256SUMS.txt`, installs it into `%LOCALAPPDATA%\freecad-mcp\bin`
(Windows) or `~/.freecad-mcp/bin` (macOS / Linux), adds that directory to your
`PATH` and runs `freecad-mcp configure`, the setup wizard:

1. **AI clients**: pick the clients to register with. Every client found on the
   machine is listed, with the ones already configured marked.
2. **FreeCAD RPC auth token**: skip this unless you set a token with
   **Set Auth Token** in FreeCAD (see [configuration](configuration.md#3-require-an-auth-token)).
3. **FreeCAD addon**: the wizard asks FreeCAD for its user data directory
   (`FreeCAD.getUserAppDataDir()`) and installs the addon into its `Mod`
   directory. The option to start the RPC server together with FreeCAD is on
   by default; turn it off with space to start the server by hand instead.
4. **Registration**: the `freecad` server is added to each selected client's
   configuration, and the wizard tells you which clients need a restart.

Restart FreeCAD and your AI client when the wizard finishes.

### Unattended install

`freecad-mcp install --yes` does the same without prompts: it installs the
addon (with auto-start on) and registers every detected client that is not
configured yet. Add `--all` to re-register configured clients, `--clients
claude-desktop,cursor` to pick clients, `--token <token>` to store the RPC auth
token, and `--dry-run` to see the plan without writing anything. To pass these
through the install script:

```powershell
& ([scriptblock]::Create((irm https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.ps1))) -ConfigureArgs "--yes"
```

```sh
curl -fsSL https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.sh | CONFIGURE_ARGS="--yes" sh
```

### Commands

| Command | Purpose |
| --- | --- |
| `freecad-mcp` | In a terminal: an interactive menu (doctor, addon install, connection check). Started by an AI client: the MCP server. |
| `freecad-mcp mcp` | Run the MCP server over stdio. |
| `freecad-mcp install` / `configure` | The setup wizard (`--yes` for unattended). |
| `freecad-mcp add` | Register the server in the current project's client configs instead of the global ones. |
| `freecad-mcp uninstall` | Remove the server from the AI clients. |
| `freecad-mcp install-addon` | Install or update the FreeCAD addon (`--user-data-dir <dir>`, `--no-autostart`, `--dry-run`). |
| `freecad-mcp uninstall-addon` | Remove the addon from FreeCAD. |
| `freecad-mcp check-connection` | Check that FreeCAD's RPC server answers and that the addon matches. |
| `freecad-mcp login --token <token>` | Store the FreeCAD RPC auth token. |
| `freecad-mcp doctor` | Check the binary, PATH, AI clients, FreeCAD, the addon and the RPC server. |
| `freecad-mcp update` | Update the binary from GitHub releases, then the addon it ships. |

## Start the RPC server

With auto-start on, the RPC server starts when FreeCAD finishes loading. To
start it by hand, select **MCP Addon** from the workbench list:

![MCP Addon in the workbench list](../assets/workbench_list.png)

and click **Start RPC Server** in the **FreeCAD MCP** toolbar:

![Start RPC Server toolbar button](../assets/start_rpc_server.png)

The command displays its result in the status bar and Report View. A successful
start includes the listening address, port, and FreeCAD process ID; by default
the address is `127.0.0.1:9875`. Startup errors include the exception, such as an
address already in use. A failed start releases its listener so you can retry
after correcting the cause.

Examples from a Linux smoke test: [successful startup](../assets/rpc-startup-success.png)
and [reported startup failure](../assets/rpc-startup-error.png).

See [auto-start configuration](configuration.md#auto-start-rpc-server) to change
the setting later.

### Keep the addon and server in sync

The binary embeds the addon, and `install`, `install-addon` and `update` install
the matching copy, so the two normally match. If they do not, for example after
copying an addon by hand, the next tool reply starts with a warning that says
which side to update, `get_rpc_status` reports it under `version_check`, and
`freecad-mcp doctor` fails its addon check. Run `freecad-mcp install-addon` and
restart FreeCAD.

## Verify the installation

```sh
freecad-mcp doctor
```

checks the binary and `PATH`, which AI clients have the server registered, the
FreeCAD installation, the installed addon version and its auto-start setting,
and whether the RPC server answers. With FreeCAD running,

```sh
freecad-mcp check-connection
```

pings the RPC server and compares the addon's version with the server's. If the
port is closed, start the RPC server and check FreeCAD's Report View. If it is
open but the check fails, check for another process using port 9875. For remote
installations, also check the [allowed IP configuration](configuration.md#remote-connections).

To inspect the listener from FreeCAD's Python console:

```python
from rpc_server import rpc_server as bridge
print(bridge.rpc_server_instance.server_address if bridge.rpc_server_instance else "RPC server stopped")
```

After connecting, ask the client to create a new document with a small
`Part::Box`. Confirm that the box appears in FreeCAD and that `list_documents`
and `get_objects` return it. The addon provides CAD tools; an MCP client is
still responsible for the conversation and the model's tool-calling loop.

## Install the addon by hand

The installer normally does this. To install the addon yourself, copy the
`addon/FreeCADMCP` directory of this repository into FreeCAD's user addon
directory, so that the result is `Mod/FreeCADMCP`, and restart FreeCAD. Do not
install anything into FreeCAD's bundled Python.

### Addon directory

| Platform / installation | Directory |
| --- | --- |
| Windows, FreeCAD 1.1 | `%APPDATA%\FreeCAD\v1-1\Mod\` |
| Windows, older unversioned installations | `%APPDATA%\FreeCAD\Mod\` |
| macOS, FreeCAD 1.1 | `~/Library/Application Support/FreeCAD/v1-1/Mod/` |
| macOS, FreeCAD 1.0 | `~/Library/Application Support/FreeCAD/v1-0/Mod/` |
| Linux, Ubuntu | `~/.FreeCAD/Mod/` |
| Linux, Snap | `~/snap/freecad/common/Mod/` |
| Linux, Debian | `~/.local/share/FreeCAD/Mod/` |
| Linux, Arch / CachyOS (FreeCAD 1.1 from `extra/freecad`) | `~/.local/share/FreeCAD/v1-1/Mod/` |
| Linux, Flatpak | `~/.var/app/org.freecad.FreeCAD/data/FreeCAD/v1-1/Mod/` |

Paths depend on the FreeCAD version and packaging. To find the user addon
directory of the running installation, open **View → Panels → Python console**
in FreeCAD and run:

```python
import os
print(os.path.join(FreeCAD.getUserAppDataDir(), "Mod"))
```

`freecad-mcp install-addon --user-data-dir <dir>` installs into a directory you
name, where `<dir>` is what `FreeCAD.getUserAppDataDir()` prints (without
`Mod`). On Windows, the directory must contain `FreeCADMCP\InitGui.py` directly,
for example `%APPDATA%\FreeCAD\v1-1\Mod\FreeCADMCP\InitGui.py`. If the workbench
is missing after restarting FreeCAD, open **View → Panels → Report view** and
inspect addon import errors.

## Connect an MCP client by hand

The wizard registers the server for you. For a client it does not know, add an
entry that runs the binary with the `mcp` argument, using its absolute path if
the client does not see your `PATH`:

```json
{
  "mcpServers": {
    "freecad": {
      "command": "freecad-mcp",
      "args": ["mcp"]
    }
  }
}
```

The MCP server talks to its client over standard input/output. Port 9875 is the
addon's XML-RPC endpoint, so do not configure an HTTP/SSE MCP client to connect
directly to that port. To serve MCP over Streamable HTTP instead, set
`TRANSPORT=http` and `ADDR=host:port`.

## Run from source

Development needs [Go](https://go.dev/dl/) (see `go.mod` for the version). From
the cloned repository:

```sh
go vet ./... && go test ./...
go run . doctor
go build -o freecad-mcp .     # freecad-mcp.exe on Windows
```

`go build` embeds the addon from `addon/FreeCADMCP`, so `./freecad-mcp
install-addon` installs your working copy. Register the development build with
your clients with `./freecad-mcp install`, which records the binary's absolute
path. The addon's own tests are Python:

```sh
python -m pip install pytest
python -m pytest tests
```

Releases are built by GoReleaser when a `v*` tag is pushed.
