[![MseeP.ai Security Assessment Badge](https://mseep.net/pr/neka-nat-freecad-mcp-badge.png)](https://mseep.ai/app/neka-nat-freecad-mcp)

# FreeCAD MCP

Control FreeCAD from Claude Desktop and other MCP clients. Create and edit models,
run Python scripts, inspect documents, and run FEM analyses.

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
irm https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.ps1 | iex
```

macOS / Linux:

```sh
curl -fsSL https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.sh | sh
```

The installer downloads `freecad-mcp`, verifies its SHA256 checksum, puts it on
your `PATH` and starts the setup wizard, which:

1. finds the AI clients on your machine (Claude Desktop, Claude Code, Cursor,
   VS Code, Windsurf, Zed and more) and lets you pick the ones to register with;
2. optionally stores the FreeCAD RPC auth token, if you set one in FreeCAD;
3. asks FreeCAD where its addons live and installs the addon there, with the
   RPC server set to start together with FreeCAD;
4. registers the `freecad` server with the selected clients.

Restart FreeCAD and your AI client, then ask it to create a model. Connections
use `localhost` by default.

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
| [Tools](docs/tools.md) | Available tools, screenshots, FEM analysis |
| [Code execution](docs/execution.md) | GUI execution, background jobs, headless scripts, timeout troubleshooting |
| [Demos and examples](docs/examples.md) | Design demos, FEM example, ADK and LangChain integrations |

## Contributors

<a href="https://github.com/neka-nat/freecad-mcp/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=neka-nat/freecad-mcp" />
</a>

Made with [contrib.rocks](https://contrib.rocks).
