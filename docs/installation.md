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
irm https://github.com/sairaph/freecad-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/sairaph/freecad-mcp/releases/latest/download/install.sh | sh
```

The script downloads the binary for your platform, verifies it against the
release's `SHA256SUMS.txt` (and installs nothing if it cannot), installs it into
`%LOCALAPPDATA%\freecad-mcp\bin` (Windows) or `~/.freecad-mcp/bin` (macOS /
Linux), adds that directory to your `PATH` and runs `freecad-mcp configure`,
the setup wizard. On macOS and Linux the `PATH` entry goes into your shell
profiles (`.zshrc`, `.bashrc`, `.profile`, `.bash_profile`, and a new `.zshrc`
when zsh is your shell and has none), skipping any you cannot write and a
`.zshrc` that is a symlink; when none
of them could take it, the script
prints the line to add to yours. The wizard:

1. **AI clients**: pick the clients to register with. Every client found on the
   machine is listed and preselected; move with the arrow keys and press space
   to deselect one. A client whose `freecad` entry you edited by hand (for
   example to add [settings](configuration.md#environment-variables)), or
   whose `freecad` entry runs another program, starts deselected so that
   entry is kept; selecting it replaces the entry. With no client selected,
   setup continues and installs only the addon.
2. **FreeCAD RPC auth token**: skip this unless you set a token with
   **Set Auth Token** in FreeCAD (see [configuration](configuration.md#3-require-an-auth-token)).
   Without a token, any program running on your machine can call FreeCAD's
   RPC server. On the input screen every key goes into the token, `q`
   included; `Esc` returns to the **Sign in** / **Skip for now** choice.
3. **FreeCAD addon**: the wizard asks FreeCAD for its user data directory
   (`FreeCAD.getUserAppDataDir()`) and shows where the addon will go. The
   option to start the RPC server together with FreeCAD starts from the
   current setting, and is on for a new install; toggle it with space. What
   you choose is applied, on or off.
4. **Registration**: the `freecad` server is added to each selected client's
   configuration, and the wizard tells you which clients need a restart. Then
   the token is saved and the addon installed.

Nothing is written before step 4. Press `q` or `Ctrl+C` in steps 1 to 3 to
cancel (on the token input screen, `Ctrl+C`): the wizard changes nothing, and
the install script removes the binary and the `PATH` entry it just added, or
puts back the version you had installed before. Interrupting the install
script before the wizard starts, or an error that stops it there, undoes its
changes the same way. Once step 4 starts writing the client configurations it runs to the
end, and setup is complete. Restart FreeCAD and your AI client when the wizard
finishes.

To install a specific release instead of the latest, pass `-Version`
(PowerShell) or set `VERSION` (sh):

```powershell
& ([scriptblock]::Create((irm https://github.com/sairaph/freecad-mcp/releases/download/v0.3.0/install.ps1))) -Version v0.3.0
```

```sh
curl -fsSL https://github.com/sairaph/freecad-mcp/releases/download/v0.3.0/install.sh | VERSION=v0.3.0 sh
```

### Unattended install

`freecad-mcp install --yes` does the same without prompts: it installs the
addon (turning auto-start on for a new install and keeping the setting of an
existing one), registers every detected client that is not configured yet,
and points entries that run another copy of `freecad-mcp` at this one. A
client whose `freecad` entry was edited by hand, or runs another program, is
left as it is, with a note on how to replace it. `install` also runs this way,
without the wizard, when given `--token`, `--clients` or `--all`, and `add`
when given `--all` (`add` takes only `--all`, `--yes`, `--dry-run` and
`--dir`; for a project token use `install --scope project --token <token>`).

- `--all` re-registers every client, edited entries included (another
  program's entry only when you name its client).
- `--clients claude-desktop,cursor` registers exactly those clients, replacing
  whatever entry they have.
- `--token <token>` stores the RPC auth token.
- `--dry-run` shows the plan without writing anything.

To pass these through the install script, which with `--yes`, `--all`,
`--clients` or `--token` also works without a terminal (in CI, Docker or a provisioning script):

```powershell
& ([scriptblock]::Create((irm https://github.com/sairaph/freecad-mcp/releases/latest/download/install.ps1))) -ConfigureArgs "--yes --clients cursor"
```

```sh
curl -fsSL https://github.com/sairaph/freecad-mcp/releases/latest/download/install.sh | CONFIGURE_ARGS="--yes --clients cursor" sh
```

The flags are split at spaces; for a value that contains one, give PowerShell
an array instead: `-ConfigureArgs "--yes", "--token", "my token"`.

### Commands

| Command | Purpose |
| --- | --- |
| `freecad-mcp` | In a terminal: an interactive menu (doctor, addon install, connection check). Started by an AI client: the MCP server. |
| `freecad-mcp mcp` | Run the MCP server over stdio. It serves FreeCAD itself and does not bridge to other MCP servers: `--remote <url>` is refused. |
| `freecad-mcp install` / `configure` | The setup wizard (`--yes` for unattended). |
| `freecad-mcp add` | Register the server in the current project's client configs instead of the global ones. |
| `freecad-mcp uninstall` | Remove the `freecad` entries that run `freecad-mcp` from the AI clients' global configurations, including entries edited by hand. An entry under that name that runs another program is left in place unless you name its client (`--clients cursor,zed` acts on only those; `--dry-run` previews). |
| `freecad-mcp uninstall --all` | Remove what `install` and the install scripts wrote for your user: those client entries, the FreeCAD addon from every FreeCAD data folder that holds it, the addon's settings file (which can hold the auth token) from every FreeCAD data folder that has one, the stored token, the cache, and the installed program with its `PATH` entry (`--dry-run` to preview). When a client entry cannot be removed, the program and its `PATH` entry stay, so that client is not left pointing at a deleted program; the output says why. What `add` wrote into projects stays; `--all` cannot be combined with `--scope project` or `--clients`. |
| `freecad-mcp uninstall --scope project` | Remove the entries `add` wrote to the current project (`--dir <dir>` for another project). A token `add` stored stays in `<project>/.freecad-mcp/`; delete that directory to remove it. |
| `freecad-mcp install-addon` | Install or update the FreeCAD addon in the data folder FreeCAD reports, or in every FreeCAD data folder found on disk when FreeCAD cannot be asked (`--user-data-dir <dir>` for another folder, `--no-autostart` to turn auto-start off, `--dry-run`). |
| `freecad-mcp uninstall-addon` | Remove the addon from every FreeCAD data folder that holds it (`--user-data-dir <dir>` for one folder). Its settings stay. |
| `freecad-mcp check-connection` | Check that FreeCAD's RPC server answers and speaks this server's protocol version. |
| `freecad-mcp login --token <token>` | Store the FreeCAD RPC auth token. |
| `freecad-mcp doctor` | Check the binary, PATH, AI clients, FreeCAD, the addon and the RPC server. |
| `freecad-mcp update` | Update the binary from GitHub releases, then every installed copy of the addon it ships. |
| `freecad-mcp version` | Print the version. |

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

### Let your AI client start FreeCAD

You do not have to start FreeCAD by hand every time: an AI client can call the
`start_freecad` tool, which launches FreeCAD's GUI detached from the MCP
server and starts the RPC server through a startup macro, independent of the
auto-start setting. See [starting FreeCAD from your AI client](configuration.md#starting-freecad-from-your-ai-client)
for how it decides between starting, reporting `already_running`, and
forwarding to a FreeCAD window that is already open.

### Keep the addon and server in sync

The binary embeds the addon, and `install`, `install-addon` and `update` install
the matching copy, so the two normally match. The MCP server checks that the
running addon speaks its protocol version: if it does not, for example after
copying an older addon by hand, the next tool reply starts with a warning that
says which side to update, and `get_rpc_status` and `check-connection` report
it. This release speaks protocol 3 (addon 0.3.0): `start_freecad` launches
FreeCAD itself when it is not running; documents gain full lifecycle tools
(open, save, save as, close, activate); `import_file` and `export_document`
handle CAD, mesh and 2D formats on disk; screenshots and object listings can
target a chosen document; `list_objects` and `get_object` report bounding
boxes, center of mass and validity; `recompute_document`, `check_printability`,
mesh analysis and repair, transactional `undo`/`redo`, spreadsheet cell access,
and `measure`/`get_selection` are new. An addon from an earlier release speaks
protocol 1 or 2 and gets the `freecad-mcp install-addon` warning until it is
updated.

`freecad-mcp doctor` also finds every installed copy of the addon, in the data
folder FreeCAD reports and in the other FreeCAD data folders on disk, and
compares each copy's version number and protocol with the ones this server
ships; a pre-release can share the version number but not the protocol. Its
addon check fails when the folder FreeCAD reports holds no copy or an outdated
one; run `freecad-mcp install-addon` and restart FreeCAD. An outdated copy in
another folder, for example one left for an older FreeCAD version, only warns,
naming `freecad-mcp install-addon --user-data-dir <dir>` to update it and
`freecad-mcp uninstall-addon --user-data-dir <dir>` to remove it.
`freecad-mcp update` updates every installed copy whose version or protocol
differs.

## Verify the installation

```sh
freecad-mcp doctor
```

checks the binary and `PATH`, which AI clients have the server registered, the
FreeCAD installation (both `freecadcmd` for headless execution and the GUI
executable `start_freecad` launches), the version and auto-start setting of
every installed copy of the addon, and whether the RPC server answers. With
FreeCAD running,

```sh
freecad-mcp check-connection
```

pings the RPC server and checks that the addon speaks the server's protocol
version. If the
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
and `list_objects` return it. The addon provides CAD tools; an MCP client is
still responsible for the conversation and the model's tool-calling loop.

## Install the addon by hand

The installer normally does this. To install the addon yourself, copy the
`addon/FreeCADMCP` directory of this repository into FreeCAD's user addon
directory, so that the result is `Mod/FreeCADMCP`, and restart FreeCAD. The
`Mod` directory must contain `FreeCADMCP\InitGui.py` directly, for example
`%APPDATA%\FreeCAD\v1-1\Mod\FreeCADMCP\InitGui.py` on Windows, not a nested
copy such as `Mod\FreeCADMCP\FreeCADMCP\InitGui.py`. Do not install anything
into FreeCAD's bundled Python.

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
`Mod`). If the workbench is missing after restarting FreeCAD, open
**View → Panels → Report view** and inspect addon import errors.

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
