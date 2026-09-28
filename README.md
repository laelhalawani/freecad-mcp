# FreeCAD MCP

Control FreeCAD from Claude Desktop and other MCP clients. Create and edit models,
import and export files, run Python scripts, inspect and measure documents,
check printability and repair meshes, and run FEM analyses.

## Demo

Design a flange:

![Designing a flange in FreeCAD](./assets/freecad_mcp4.gif)

See [more demos and examples](docs/examples.md) for a toy car, modelling from a
2D drawing, and agent integrations.

## Quick start

You only need [FreeCAD](https://www.freecad.org/downloads.php). FreeCAD MCP is a
single self-contained binary: it needs no Python, uv or pip on your machine. The
part that runs inside FreeCAD is an addon executed by FreeCAD's own bundled
Python, and the binary installs it for you.

Windows (PowerShell):

```powershell
irm https://github.com/sairaph/freecad-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/sairaph/freecad-mcp/releases/latest/download/install.sh | sh
```

The installer downloads `freecad-mcp`, verifies its SHA256 checksum, puts it on
your `PATH` and starts the setup wizard, which:

1. finds the AI clients on your machine (Claude Desktop, Claude Code, Cursor,
   VS Code, Windsurf, Zed and more) and lets you pick the ones to register with;
2. optionally stores the FreeCAD RPC auth token, if you set one in FreeCAD;
3. asks FreeCAD where its addons live and shows where the addon will go, with
   the option to start the RPC server together with FreeCAD (on for a new
   install, otherwise as you last set it);
4. registers the `freecad` server with the selected clients, then saves the
   token and installs the addon.

Nothing is written until step 4, so cancelling earlier leaves your machine as
it was.

Restart FreeCAD and your AI client, then ask it to create a model. From then
on, your AI client can start FreeCAD itself with the `start_freecad` tool
whenever it is not already running. Connections use `localhost` by default.
Without an auth token, any program running on your machine can call FreeCAD's
RPC server; see [configuration](docs/configuration.md#3-require-an-auth-token)
to require one.

Run `freecad-mcp doctor` at any time to check the installation, and
`freecad-mcp update` to update the server together with the addon it ships
(restart FreeCAD afterwards). See the
[installation guide](docs/installation.md) for unattended installs, manual
setup and troubleshooting.

## Documentation

| Guide | Contents |
| --- | --- |
| [Installation](docs/installation.md) | Installer, commands, addon directories, running from source |
| [Configuration](docs/configuration.md) | Environment variables, auto-start, text feedback, remote connections |
| [Tools](docs/tools.md) | Available tools, screenshots, file import/export, printability and mesh checks, FEM analysis |
| [Code execution](docs/execution.md) | GUI execution, background jobs, headless scripts, timeout troubleshooting |
| [Demos and examples](docs/examples.md) | Design demos, FEM example, ADK and LangChain integrations |

## Contributors

<a href="https://github.com/sairaph/freecad-mcp/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=sairaph/freecad-mcp" />
</a>

Made with [contrib.rocks](https://contrib.rocks).

## Credits

This project was forked and reworked from [neka-nat/freecad-mcp](https://github.com/neka-nat/freecad-mcp).
