package main

// The one-shot commands of remote access: `listen` runs the listener,
// `share` turns "Share this PC" on or off, and `connect` saves or clears
// FreeCAD on another computer.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sairaph/mcp-wizard/command"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
)

// readPasswordStdin reads one line from standard input for --password-stdin,
// so a password never appears on the command line (visible to other local
// users through ps or /proc, and left in shell history).
func readPasswordStdin() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// shareNoAddonMessage is share's "no installed addon" refusal, the same
// wording the Share page uses (app_share.go) with the CLI command in place
// of its menu path.
func shareNoAddonMessage() string {
	return "  No installed FreeCAD addon was found; install it first (`" + domain.BinaryName + " install-addon`)."
}

func registerRemoteCommands(r *command.Registry) {
	r.Register(command.Handler{
		Name:        "listen",
		Description: "Run the listener that shares FreeCAD with other devices (started when you log in, by Share this PC)",
		Usage:       "listen [--user-data-dir <dir>] [--rpc-port 9875] [--freecad <cmd>]",
		Run:         runListen,
	})
	r.Register(command.Handler{
		Name:        "share",
		Description: "Share this PC's FreeCAD with other devices, or stop sharing it",
		Usage:       "share --on|--off [--allowed-ips <list>] [--password <password> | --password-stdin | --no-password] [--session-timeout <minutes>] [--port 9876]",
		Run:         runShare,
	})
	r.Register(command.Handler{
		Name:        "connect",
		Description: "Use FreeCAD on another computer that shares it, or use this computer again (--clear)",
		Usage:       "connect --host <host> [--port 9876] [--password <password> | --password-stdin] | --clear",
		Run:         runConnect,
	})
}

// runShare is the `share` command. Every flag not passed on the command
// line keeps the value already saved (the first time, before anything is
// saved, that is the suggested default: the default-route subnet, no
// password, 30 minutes, port 9876).
func runShare(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	on := fs.Bool("on", false, "turn remote access on")
	off := fs.Bool("off", false, "turn remote access off")
	allowedIPs := fs.String("allowed-ips", "", "comma-separated allowed IP addresses or subnets (default: keep the current value, or this computer's default-route subnet the first time)")
	password := fs.String("password", "", "password other devices must give; visible to other local users on this command line and in shell history, so --password-stdin is preferred (default: keep the current value, or none the first time)")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from standard input (one line)")
	noPassword := fs.Bool("no-password", false, "remove the password")
	timeout := fs.Int("session-timeout", 0, "minutes to keep the session assigned to an agent, 1 to 1440 (default: keep the current value, or 30 the first time)")
	port := fs.Int("port", 0, "listener port, 1 to 65535 (default: keep the current value, or 9876 the first time)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *on == *off {
		fmt.Fprintln(os.Stderr, "  share: pass exactly one of --on or --off")
		return 2
	}
	targets := addoninstall.LocateInstalled(ctx, freecadCommand())
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, shareNoAddonMessage())
		return 1
	}
	if *off {
		return applyUnshare(ctx, os.Stdout, targets)
	}

	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	if passed["password"] && *passwordStdin {
		fmt.Fprintln(os.Stderr, "  share: pass exactly one of --password or --password-stdin")
		return 2
	}
	if *passwordStdin {
		v, err := readPasswordStdin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  share: reading --password-stdin: %v\n", err)
			return 2
		}
		*password = v
		passed["password"] = true
	}
	if *noPassword && passed["password"] {
		fmt.Fprintln(os.Stderr, "  share: pass exactly one of --password (or --password-stdin) or --no-password")
		return 2
	}

	current, err := addoninstall.ReadRemoteSettings(targets[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [fail] %v\n", err)
		return 1
	}

	ips := current.AllowedIPs
	switch {
	case passed["allowed-ips"]:
		ips = strings.TrimSpace(*allowedIPs)
	case ips == "":
		// Never saved: there is no custom value worth keeping, suggest one
		// instead.
		ips = strings.Join(suggestedAllowedIPs(), ", ")
	case !current.RemoteEnabled && ips == addoninstall.DefaultAllowedIPs:
		// Remote access is off and nothing but the plain default was ever
		// saved: still nothing deliberately chosen, suggest one instead
		// (live check L8). While remote access is already on, "127.0.0.1"
		// is instead the loopback-only SSH-tunnel setup (contract 5.1),
		// chosen on purpose, not left over from never having shared yet, so
		// it must not be replaced with the LAN subnet just because a flag
		// such as --password-stdin changed something else (live-fixes
		// review M2: L8's fix over-corrected by dropping this condition
		// entirely). A list other than the plain default is kept either
		// way, so turning sharing off and back on never drops a custom one.
	}
	if _, err := domain.ParseAllowedIPs(ips); err != nil {
		fmt.Fprintf(os.Stderr, "  share: --allowed-ips: %v\n", err)
		return 2
	}

	pw := current.AuthToken
	switch {
	case *noPassword:
		pw = ""
	case passed["password"]:
		pw = *password
	}
	if strings.TrimSpace(pw) != pw {
		fmt.Fprintln(os.Stderr, "  share: the password cannot start or end with a space")
		return 2
	}

	timeoutMinutes := current.SessionTimeoutMinutes
	if passed["session-timeout"] {
		timeoutMinutes = *timeout
	}
	if timeoutMinutes < addoninstall.MinSessionTimeoutMinutes || timeoutMinutes > addoninstall.MaxSessionTimeoutMinutes {
		fmt.Fprintf(os.Stderr, "  share: --session-timeout must be %d to %d minutes\n", addoninstall.MinSessionTimeoutMinutes, addoninstall.MaxSessionTimeoutMinutes)
		return 2
	}

	listenerPort := current.ListenerPort
	if passed["port"] {
		listenerPort = *port
	}
	if listenerPort < 1 || listenerPort > 65535 {
		fmt.Fprintln(os.Stderr, "  share: --port must be 1 to 65535")
		return 2
	}

	opts := shareOptions{AllowedIPs: ips, Password: pw, TimeoutMinutes: timeoutMinutes, Port: listenerPort}
	return applyShare(ctx, os.Stdout, targets, opts)
}

// runConnect is the `connect` command.
func runConnect(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	host := fs.String("host", "", "host name or IP address of the computer that runs FreeCAD")
	port := fs.Int("port", addoninstall.DefaultListenerPort, "listener port")
	password := fs.String("password", "", "password set in Share this PC on that computer; visible to other local users on this command line and in shell history, so --password-stdin is preferred")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from standard input (one line)")
	clear := fs.Bool("clear", false, "use FreeCAD on this computer again")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *passwordStdin && *password != "" {
		fmt.Fprintln(os.Stderr, "  connect: pass exactly one of --password or --password-stdin")
		return 2
	}
	if *passwordStdin {
		v, err := readPasswordStdin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  connect: reading --password-stdin: %v\n", err)
			return 2
		}
		*password = v
	}
	if *clear {
		if strings.TrimSpace(*host) != "" || *password != "" {
			fmt.Fprintln(os.Stderr, "  connect: --clear does not take --host or --password")
			return 2
		}
		if err := clearConnection(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "  connect: %v\n", err)
			return 1
		}
		fmt.Println("  Removed. AI clients use FreeCAD on this computer the next time they start freecad-mcp.")
		return 0
	}

	h := strings.TrimSpace(*host)
	if h == "" {
		fmt.Fprintln(os.Stderr, "  connect: --host is required")
		return 2
	}
	if err := domain.ValidateHost(h); err != nil {
		fmt.Fprintf(os.Stderr, "  connect: %v\n", err)
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "  connect: --port must be 1 to 65535")
		return 2
	}
	if strings.TrimSpace(*password) != *password {
		fmt.Fprintln(os.Stderr, "  connect: the password cannot start or end with a space")
		return 2
	}

	result := testConnect(ctx, h, *port, *password)
	lines, ok := connectResultLines(h, *port, *password, result)
	for _, l := range lines {
		fmt.Println("  " + l)
	}
	if !ok {
		return 1
	}
	if err := saveConnection(ctx, h, *port, *password); err != nil {
		fmt.Fprintf(os.Stderr, "  connect: %v\n", err)
		return 1
	}
	fmt.Println("  Saved. AI clients use it the next time they start freecad-mcp (restart them).")
	return 0
}

// connectResultLines renders r as the result lines printed after testing a
// connection to host:port, reusing the install wizard's
// connectSuccessLine/connectErrLine so a refusal from a listener that
// answered (403, 429, or any other marker-present status) gets its own
// reason instead of "Could not reach", which stays for a genuine network
// failure. ok reports whether the connection was usable (a listener answered
// 200), which is when connect saves it. password is the one that was sent
// (possibly empty), so a 401 without one is told to pass --password rather
// than "was not accepted", which implies a wrong one was tried.
func connectResultLines(host string, port int, password string, r connectResult) (lines []string, ok bool) {
	switch {
	case r.PasswordRequired && password == "":
		return []string{fmt.Sprintf(
			"[fail] %s asks for a password: the one set in \"Share this PC\" on that computer. "+
				"Pass it with --password <password> or --password-stdin.", host)}, false
	case r.PasswordRequired:
		return []string{passwordNotAcceptedLine}, false
	case r.NotListener:
		return []string{notListenerLine(host, port)}, false
	case r.Err != nil:
		return []string{connectErrLine(r.Err, host, port)}, false
	default:
		lines = append(lines, connectSuccessLine(r.Status, host, port))
		if w := protocolWarningLine(r.Status); w != "" {
			lines = append(lines, w)
		}
		return lines, true
	}
}
