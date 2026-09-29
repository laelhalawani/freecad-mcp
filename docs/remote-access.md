# Remote access

[Back to README](../README.md) · [Configuration](configuration.md) · [Tools](tools.md)

By default FreeCAD MCP only talks to FreeCAD on the same machine. Remote
access lets an AI client on one computer control FreeCAD running on another:
install freecad-mcp on both, turn "Share this PC" on where FreeCAD runs, then
use "Connect to FreeCAD on another computer" everywhere else.

## What Share this PC and Connect do

- **Share this PC**, on the computer that runs FreeCAD, turns on a small
  listener that other devices can reach, sets which IP addresses may connect
  (under Advanced), and optionally sets a password. Open it from `freecad-mcp`
  (run with no arguments in a terminal) or from the install wizard's own step
  (which leaves the allowed addresses to the automatic choice); the
  one-shot command is `freecad-mcp share`.
- **Connect to FreeCAD on another computer**, on every other computer, points
  that computer's freecad-mcp at the shared one: host, port and password. AI
  clients on that computer then reach FreeCAD through the listener instead of
  a local addon. Open it the same way; the one-shot command is
  `freecad-mcp connect`.

Both are always available as separate pages of the app's menu (plain
`freecad-mcp`). The install wizard (`freecad-mcp install` / `configure`)
shows only one of them as a step, depending on what it found: Share this PC
when it found FreeCAD on this computer, or the "FreeCAD on another computer"
step (what Connect also does) when it did not. Either way most people never
need the command-line form; it exists for scripted or unattended setups.

## The listener

Sharing works through a listener process, `freecad-mcp listen`, separate from
the FreeCAD addon:

- The addon's own RPC server still binds `127.0.0.1` only, on port 9875 by
  default. It never talks to the network directly.
- The listener binds the network, on port 9876 by default, and forwards
  requests to the addon over loopback. It also answers status checks and can
  start FreeCAD itself (see below), so remote agents work even before
  anyone opens FreeCAD.
- The listener starts automatically when you log in to the computer, through
  whatever your operating system uses for that: Task Scheduler on Windows, a
  launch agent on macOS, a systemd user service (or a plain autostart entry
  where systemd is not available) on Linux. Turning "Share this PC" on
  registers it for that and starts it right away; turning it off stops it
  and removes the registration. It restarts itself if it crashes.
- Remote `start_freecad` works the same way as local `start_freecad`: an AI
  client on another computer can start FreeCAD's GUI on the shared computer
  and open a file in it, without anyone sitting at that computer first.

The addon's port is 9875; it is not a FreeCAD setting, so `start_freecad`
sets another through `FREECAD_MCP_PORT` when it needs to, and the listener
is then told about it with `--rpc-port`. The listener's own port is
configurable: Share this PC's Port field, or `freecad-mcp share --port`.

## Settings

Share this PC has four settings, saved to the addon's settings file on the
FreeCAD computer:

| Setting | Default | Meaning |
| --- | --- | --- |
| Allowed IPs | the saved list, else the local network's subnet found from this computer's own address (127.0.0.1 when no network is found) | Comma-separated IP addresses or CIDR subnets allowed to connect. Loopback addresses (127.0.0.1, ::1) are always allowed in addition to this list, whatever it says. The install wizard sets it without asking; change it in Share this PC > Advanced (a collapsed row showing the allowed devices and the port; space or enter expands it into the Allowed IPs and Port fields), or with `freecad-mcp share --allowed-ips`. |
| Password | none | Sent by every connecting client; required whenever it is not empty. The same password also protects the local addon, so agents on the shared computer need it too once one is set. |
| Port | 9876 | The listener's own port. |
| Session timeout | 30 minutes | How long an idle agent keeps FreeCAD before another agent can take it; see [multi-agent rules](#multi-agent-rules). |

A password is rejected if it starts or ends with a space, since it could
never be typed back correctly. Changing the password or the allowed list
applies at once, except that switching the list between loopback only
(127.0.0.1) and a network list needs the listener restarted, which Share
this PC's Save does for you.

## Security

Remote access trades some safety for convenience. Before turning it on:

- The connection is plain HTTP: nothing is encrypted, including the
  password. Anyone who can watch the network between the two computers can
  read it.
- With no password set, anyone on an allowed IP address can run arbitrary
  Python inside FreeCAD through `execute_code`, with the same permissions as
  whoever is logged in to the FreeCAD computer. Set a password whenever you
  turn sharing on, and keep the allowed list as narrow as you can.
- Loopback is always allowed, whatever the allowed list says: any program
  running on the FreeCAD computer itself can always reach the listener and
  the addon; only the password stands between it and FreeCAD. This is
  intentional (local tools should keep working) but worth remembering on a
  shared machine.
- The multi-agent session lock (below) only coordinates well-behaved
  clients; it is not a security boundary. Any client that has the password
  can send another agent's session id and take over its session. Do not
  treat it as isolation between users, only as a way to stop two agents
  from stepping on each other by accident.
- When the FreeCAD computer uses a VPN for all its traffic, the suggested
  allowed list is the VPN's own subnet, since that is where its default
  route now goes. If the other devices actually reach it over the local
  network, replace the suggestion with the local network's subnet (for
  example 192.168.1.0/24); if they connect through the same VPN, keep the
  suggestion and use the FreeCAD computer's VPN address in Connect.

On an untrusted network, use the SSH tunnel below instead of exposing the
listener directly.

## SSH tunnel

To reach FreeCAD on another computer without exposing the listener to the
network at all, tunnel it over SSH instead:

1. On the FreeCAD computer, turn on Share this PC with allowed IPs set to
   `127.0.0.1` only, and set a password (see [security](#security) above).
   The install wizard picks the local network's subnet without asking, so
   set the list afterwards: open `freecad-mcp` > Share this PC > Advanced,
   type `127.0.0.1` in Allowed IPs and save, or run
   `freecad-mcp share --on --allowed-ips 127.0.0.1` with a password option.
   A saved `127.0.0.1` is kept while sharing stays on. The listener then binds
   loopback alone.
2. On the computer that will run the AI client, forward a local port to the
   FreeCAD computer's listener over SSH:

   ```sh
   ssh -N -L 9876:127.0.0.1:9876 user@freecad-host
   ```

   If port 9876 is already taken on this computer, for example because it
   shares its own FreeCAD too, forward a different local port instead
   (`-L 9877:127.0.0.1:9876`) and use that one below.
3. Point freecad-mcp at the tunnel's local end, typing the password when
   asked:

   ```sh
   freecad-mcp connect --host 127.0.0.1 --port 9876 --password-stdin
   ```

   Leave out `--password-stdin` when no password is set.

Everything now travels encrypted inside the SSH connection, and the listener
never accepts a connection that did not come through it.

## Multi-agent rules

With remote access on, more than one agent (or AI client) can reach the same
FreeCAD at once, so a session lock keeps them from writing to it at the same
time:

- The first call any agent makes, other than `start_freecad` and
  `get_rpc_status`, claims FreeCAD for that agent. Every following call from
  that agent renews the claim; a call from another agent while it is held
  fails with an "in use" error naming the holder, how long they have been
  idle, and when the lock frees on its own.
- `get_rpc_status` always answers, whoever holds the lock, and reports who
  holds it, whether they are idle or actively working, and when it frees.
  Poll it before retrying a call that failed because FreeCAD was in use.
- `release_session` frees the calling agent's own claim as soon as it is
  done, so the next agent does not have to wait for the idle timeout. It
  only ever releases the caller's own session.
- `close_freecad` quits FreeCAD on the shared computer and frees the lock
  with it; it refuses while there are unsaved changes (pass
  `discard_changes: true` to quit anyway) or while a task panel or command
  is open in FreeCAD. `start_freecad` can start it again afterwards.
- An idle claim frees itself automatically after the session timeout (30
  minutes by default), so a crashed or disconnected agent does not lock
  FreeCAD forever.
- The person sitting at the FreeCAD computer can always take FreeCAD back
  with the **Force release** button on the status bar widget, whatever any
  agent is doing.
- Because of all this, an agent should save its work and call
  `release_session` or `close_freecad` when it is done or about to be idle
  for a while, rather than leaving FreeCAD claimed and unsaved.

The lock is cooperative, not a boundary: see [security](#security) above.

## File paths

Every tool that takes a file path (opening, saving, importing, exporting, a
script's own file access) works with paths on the computer that runs
FreeCAD, which with remote access is not the same computer as the one
running the AI client or the MCP server. `execute_code_headless` is the
exception: its script runs on the machine hosting the MCP server, so its
paths are on that computer instead. This connection never transfers files:
to get a file onto or off the FreeCAD computer, copy it there by other
means, for example `scp`, a shared drive, or a sync tool, before or after
asking an agent to use it.

## macOS: Local Network prompt

macOS is not tested live in this release, but on general macOS behavior: the
first time the listener binds to the network, macOS may ask whether
`freecad-mcp` can find devices on the local network. If it asks, allow it;
denying it, or dismissing the prompt, may keep other computers from reaching
the listener even though it is running. If this happens, check
**System Settings > Privacy & Security > Local Network**.

## WSL and Windows on the same computer

Running freecad-mcp on both Windows and inside WSL on the same PC, sharing
FreeCAD from one or both sides, is unusual but works, with one thing to
know: WSL's own networking (in its default NAT mode) forwards `localhost`
on Windows into WSL for any port something inside WSL listens on. If a
listener or an addon is also sharing FreeCAD inside WSL on the same ports
(9875, 9876), a Windows client connecting to `localhost` can be captured by
WSL's forwarding instead of reaching Windows' own FreeCAD or listener, so
"FreeCAD" turns out to be the wrong computer's.

This server catches it when it happens, once, the first time it connects:
FreeCAD's reply carries the hostname of the computer actually running it and
which platform that is (and whether that is WSL), and when this server is
configured for a loopback host, it compares those against its own. Since WSL
takes the Windows computer's own name by default, both sides can report the
same hostname even when one of them is actually the wrong one, so both
checks run; either one differing is enough. When they do, every call is
refused, not only `get_rpc_status`, naming the answering side as, for
example, "linux (WSL) on DESKTOP-ABC1" so it reads clearly as not this
computer; the finding is cached for as long as that connection lives, so
fixing the setup needs a fresh one (restart the AI client) to be picked up.

A password prompt can carry the same cause: a wrong or missing password
(HTTP 401) from a loopback address on Windows may mean WSL's forwarding
answered instead of the Windows side's own addon, each with its own separate
password; the error then says so and points back here.

The same forwarding also keeps FreeCAD's own RPC server from starting at
all: FreeCAD on Windows cannot open its RPC port (9875 by default) while
WSL's forwarding already holds `127.0.0.1:9875` for the WSL side (the addon
binds that port exclusively, so it fails to start there loudly, with a
Report View line naming the port, rather than quietly sharing it). There
are two ways out of that: turn WSL's `localhost` forwarding off
(`localhostForwarding=false` in `.wslconfig`, which needs `wsl --shutdown`
to take effect), or give the WSL side its own RPC port instead of the
addon's default one (`--rpc-port` for the addon there).

Turning sharing off on one side never reaches the other: `freecad-mcp share
--off` in WSL stops sharing FreeCAD from inside WSL, but it does not close
the FreeCAD running there. If the point was to free the port for the
Windows side, close that FreeCAD in WSL too (or quit it from within
FreeCAD itself), not only turn its sharing off.

To avoid the whole class of problem:

- Do not share FreeCAD (or run the addon) on the same ports on both sides of
  the same PC at once. Give one side a different port (Share this PC's Port
  field, or `--rpc-port` for the addon) if you need both.
- Or point one side's freecad-mcp at the other's real address instead of
  `localhost`: WSL's own address from Windows (`ip addr show eth0` inside
  WSL), or the WSL host's own LAN address from inside WSL.
- Or turn WSL's `localhost` forwarding off for the distribution that is not
  meant to answer it (`localhostForwarding=false` in `.wslconfig`, which
  needs `wsl --shutdown` to take effect), so only the side you mean to use
  answers `localhost` at all.

## Troubleshooting

```sh
freecad-mcp doctor
```

reports, on the computer sharing FreeCAD, whether the listener is
registered to start at login, actually running, and answering on its own
port; and, on a computer connected to another one, whether that computer's
listener is reachable and what it reports. Run it on whichever side is not
working.

Other things to check:

- The FreeCAD computer's firewall must allow incoming connections on the
  listener's port (9876 by default). On Windows, the listener's first start
  usually prompts for this; allow it for the network type this computer
  uses (Private or Public). Blocking or dismissing that prompt leaves a
  Block rule for freecad-mcp in place, which then refuses every later start
  too, prompt or no prompt: open **Windows Defender Firewall with Advanced
  Security**, look in **Inbound Rules** for one named freecad-mcp, and
  delete it (or turn Action to Allow) to get the prompt again.
- The connecting computer's IP address must be in the FreeCAD computer's
  allowed list. An address that is not in the list gets no answer at all,
  the same as a firewall block, so the connecting computer's `freecad-mcp
  doctor` or Connect reports that it could not reach the listener, not that
  it was refused. Check the list on the FreeCAD computer (Share this PC's
  Test button only tests that computer's own addresses).
- A wrong or missing password shows as an authentication failure on the
  connecting computer; set or update it from Connect (or `freecad-mcp
  connect --password-stdin`), matching what Share this PC has on the other
  side.

## Command-line

For scripted or unattended setups, `share`, `connect` and `listen` do the
same work as the app pages:

```text
freecad-mcp share --on [--allowed-ips <list>] [--password <password> | --password-stdin | --no-password] [--session-timeout <minutes>] [--port 9876]
freecad-mcp share --off
freecad-mcp connect --host <host> [--port 9876] [--password <password> | --password-stdin]
freecad-mcp connect --clear
freecad-mcp listen [--user-data-dir <dir>] [--rpc-port 9875] [--freecad <cmd>]
```

`share --on` keeps whatever is already saved for any flag you leave out, so
`freecad-mcp share --on --password-stdin` can change only the password.
`share --off` undoes `share --on`; `connect --clear` undoes `connect`.
`--password <password>` is convenient for testing but leaves the password in
your shell history and visible to other users on the machine; prefer
`--password-stdin` for anything you keep. `listen` is what "Share this PC"
registers to run at login; you do not normally run it by hand.
