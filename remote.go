package main

// Remote access logic shared by the install wizard, the app pages and the
// one-shot commands `share` and `connect`: applying and removing "Share this
// PC", testing a share or a connection, and saving or clearing the
// connection to FreeCAD on another computer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/mcp-wizard/secret"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/autostart"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/headless"
	"github.com/sairaph/freecad-mcp/internal/listener"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
)

// remoteAccessDocsURL is where the wizard and the app pages point the user
// for the remote access guide (an SSH tunnel, encryption, the session lock):
// a path in the repository ("docs/remote-access.md") that an installed user
// does not have, so every reference uses the hosted page instead.
const remoteAccessDocsURL = "https://github.com/sairaph/freecad-mcp/blob/main/docs/remote-access.md"

// hostPort renders host and port for user text the way every address in
// this program is shown: net.JoinHostPort brackets an IPv6 host ("[::1]:9876"),
// where a plain "%s:%d" would read as "::1:9876", ambiguous with a longer
// address.
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// listenerRegistrationCheckFailedLine prints autostart.Status's own error,
// wrapped the same way wherever checking the listener's registration is the
// first step before acting on it: stopListener (uninstall.go) and
// restartListenerIfRegistered (lifecycle.go).
func listenerRegistrationCheckFailedLine(w io.Writer, err error) {
	fmt.Fprintf(w, "  [fail] check the listener registration: %v\n", err)
}

// portInUseLine is testShare's message when opts.Port is already held by
// something other than a freecad-mcp listener, whether that was found by
// probing it (testShare) or by a bind failure starting the temporary test
// listener (testTempListener), so the wording exists once.
func portInUseLine(port int) string {
	return fmt.Sprintf("[fail] Port %d is used by another program; choose another port.", port)
}

// passwordNotAcceptedLine is the message for a 401 with a password already
// sent (as opposed to none sent, which asks for one instead): shared by the
// install wizard's applyConnectResult and the `connect` command's
// connectResultLines so the wording exists once.
const passwordNotAcceptedLine = "[fail] The password was not accepted. Check it in \"Share this PC\" on that computer."

// protocolWarningLine returns the "[warn] ... protocol N ..." line for a
// listener status whose protocol differs from this program's, or "" when
// they match. Shared by formatConnectTest (app_connect.go),
// applyConnectResult (wizard_connect.go) and connectResultLines
// (remote_commands.go), so the wording exists once.
func protocolWarningLine(st listenerapi.Status) string {
	if st.Protocol == domain.ProtocolVersion {
		return ""
	}
	return fmt.Sprintf(
		"[warn] That computer runs freecad-mcp %s (protocol %d); this one runs %s (protocol %d). "+
			"Update the older one.",
		st.Version, st.Protocol, version, domain.ProtocolVersion)
}

// maskValue turns a password into asterisks for display, one per rune, so
// its length is not misleading for a multi-byte character. The app's Share
// and Connect pages both call it; the install wizard's own maskedFieldDisplay
// (wizard_connect.go) uses it too, adding the wizard's unconditional trailing
// cursor mark on top.
func maskValue(s string) string {
	return strings.Repeat("*", len([]rune(s)))
}

// notListenerLine renders the message for remote.ErrNotListener, shared by
// the install wizard's applyConnectResult, the app's formatConnectTest and
// the `connect` command's connectResultLines, so the wording exists once.
func notListenerLine(host string, port int) string {
	return fmt.Sprintf(
		"[fail] %s is not a freecad-mcp listener. Update freecad-mcp there and turn on \"Share this PC\".",
		hostPort(host, port))
}

// --- Shared field validation (one wording per field, checked wherever it is
// entered: the wizard, both app pages, share, connect) ---

// validatePassword refuses a password with leading or trailing whitespace:
// the addon and the listener check it untrimmed, so such a password can
// never be typed back correctly (a Go http.Header write and the addon's own
// parsing both trim it), only trigger the backoff.
func validatePassword(password string) error {
	if strings.TrimSpace(password) != password {
		return errors.New("The password cannot start or end with a space.")
	}
	return nil
}

// validatePortValue checks an already-parsed listener port: 1 to 65535.
func validatePortValue(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("Port must be a number from 1 to 65535.")
	}
	return nil
}

// validatePort parses a listener port: an integer from 1 to 65535.
func validatePort(text string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, validatePortValue(-1)
	}
	if err := validatePortValue(port); err != nil {
		return 0, err
	}
	return port, nil
}

// validateMinutesValue checks an already-parsed session timeout: from
// addoninstall.MinSessionTimeoutMinutes to MaxSessionTimeoutMinutes.
func validateMinutesValue(minutes int) error {
	if minutes < addoninstall.MinSessionTimeoutMinutes || minutes > addoninstall.MaxSessionTimeoutMinutes {
		return fmt.Errorf("Minutes must be a number from %d to %d.",
			addoninstall.MinSessionTimeoutMinutes, addoninstall.MaxSessionTimeoutMinutes)
	}
	return nil
}

// validateMinutes parses a session timeout: an integer from
// addoninstall.MinSessionTimeoutMinutes to MaxSessionTimeoutMinutes.
func validateMinutes(text string) (int, error) {
	minutes, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, validateMinutesValue(addoninstall.MinSessionTimeoutMinutes - 1)
	}
	if err := validateMinutesValue(minutes); err != nil {
		return 0, err
	}
	return minutes, nil
}

// shareOptions are the "Share this PC" values.
type shareOptions struct {
	AllowedIPs     string // validated with domain.ParseAllowedIPs
	Password       string // "" means none
	TimeoutMinutes int    // 1 to 1440
	Port           int    // listener port, 1 to 65535
}

// applyShare turns remote access on in every target's addon settings,
// mirrors the password into the credential store (removed there when it is
// empty), registers the listener and starts or restarts it, printing one
// [ok] or [fail] line per step and carrying on after a failure. It returns
// an exit code.
func applyShare(ctx context.Context, w io.Writer, targets []addoninstall.Target, opts shareOptions) int {
	if len(targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return 1
	}
	ok := applyShareSettings(ctx, w, targets, opts, false)
	if !applyShareListenerRegistration(w, targets) {
		ok = false
	}
	if !ok {
		return 1
	}
	shareOnResultLine(w, opts.Port, opts.AllowedIPs)
	return 0
}

// shareOnResultLine prints the final "[ok] Remote access on" summary and, on
// Windows, the firewall-prompt note that follows it. Shared by applyShare
// (the `share` command) and the install wizard's applyShareListenerOn
// (wizard_share.go) so the message exists once.
func shareOnResultLine(w io.Writer, port int, allowedIPs string) {
	fmt.Fprintf(w, "  [ok] Remote access on: other devices connect to %s; allowed: %s\n",
		shareAddressLine(port), allowedIPs)
	if runtime.GOOS == "windows" {
		fmt.Fprintln(w, "  Windows may ask whether to allow freecad-mcp. Allow it for the network type this "+
			"computer uses (Private or Public), or other devices cannot connect.")
	}
}

// applyShareSettings writes opts into every target's addon settings and
// mirrors the password into the credential store (removed there when it is
// empty), printing one [ok] or [fail] line per step and carrying on after a
// failure. dryRun prints what would be saved instead of writing anything.
// It is shared by applyShare (the `share` command) and the install wizard's
// own settings-then-addon-then-listener ordering (wizard_share.go). It
// reports whether every step succeeded.
func applyShareSettings(ctx context.Context, w io.Writer, targets []addoninstall.Target, opts shareOptions, dryRun bool) bool {
	if _, err := domain.ParseAllowedIPs(opts.AllowedIPs); err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return false
	}
	if err := validatePassword(opts.Password); err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return false
	}
	if err := validateMinutesValue(opts.TimeoutMinutes); err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return false
	}
	if err := validatePortValue(opts.Port); err != nil {
		fmt.Fprintf(w, "  [fail] %v\n", err)
		return false
	}
	if dryRun {
		for _, t := range targets {
			fmt.Fprintf(w, "  would save the remote access settings in %s\n", addoninstall.SettingsPath(t))
		}
		return true
	}

	ok := true
	values := map[string]any{
		addoninstall.KeyRemoteEnabled:  true,
		addoninstall.KeyAllowedIPs:     opts.AllowedIPs,
		addoninstall.KeyAuthToken:      opts.Password,
		addoninstall.KeySessionTimeout: opts.TimeoutMinutes,
		addoninstall.KeyListenerPort:   opts.Port,
	}
	for _, t := range targets {
		if err := addoninstall.UpdateSettings(t, values); err != nil {
			fmt.Fprintf(w, "  [fail] save the remote access settings in %s: %v\n", addoninstall.SettingsPath(t), err)
			ok = false
			continue
		}
		fmt.Fprintf(w, "  [ok] Saved the remote access settings in %s\n", addoninstall.SettingsPath(t))
	}

	// The credential store's token belongs to whichever computer's FreeCAD
	// the local agents actually use. With a host stored (Connect), that is
	// the other computer, not this one: writing this PC's password there
	// would send it to that host on the agents' next call. Leave the
	// credential alone in that case; "Use FreeCAD on this computer instead"
	// or `connect --clear` (see clearConnection) is what restores it.
	storedHost, hostErr := loadCredential(ctx, domain.HostKey)
	switch {
	case hostErr != nil:
		fmt.Fprintf(w, "  [fail] check the stored connection: %v\n", hostErr)
		ok = false
	case storedHost != "":
		fmt.Fprintf(w, "  [note] Agents on this computer use FreeCAD on %s. To use this computer's "+
			"FreeCAD instead, open Connect and press d.\n", storedHost)
	default:
		if err := setCredentialToken(ctx, opts.Password); err != nil {
			fmt.Fprintf(w, "  [fail] store the password: %v\n", err)
			ok = false
		} else if opts.Password == "" {
			fmt.Fprintln(w, "  [ok] Removed the stored password")
		} else {
			fmt.Fprintln(w, "  [ok] Stored the password for the agents on this computer")
		}
	}
	return ok
}

// applyShareListenerRegistration registers the listener autostart entry
// (recorded against the first located target) and starts or restarts it,
// printing one [ok] or [fail] line per step. It reports whether both steps
// succeeded.
func applyShareListenerRegistration(w io.Writer, targets []addoninstall.Target) bool {
	registered := true
	wasRunning := false

	entry, err := listenerEntry(targets)
	if err == nil {
		if state, statusErr := autostart.Status(); statusErr == nil {
			wasRunning = state.Running
		}
		err = autostart.Register(entry)
	}
	if err != nil {
		fmt.Fprintf(w, "  [fail] register the listener: %v\n", err)
		registered = false
	} else {
		fmt.Fprintln(w, "  [ok] The listener starts with this computer")
	}

	// The start/restart step always reports a line, even when registration
	// failed, so every step is accounted for and the run carries on.
	if !registered {
		fmt.Fprintln(w, "  [fail] start the listener: registration failed")
		return false
	}

	startErr := autostart.Start()
	if wasRunning {
		startErr = autostart.Restart()
	}
	if startErr != nil {
		fmt.Fprintf(w, "  [fail] start the listener: %v\n", startErr)
		return false
	}
	fmt.Fprintln(w, "  [ok] Started the listener")
	return true
}

// listenerEntry is the autostart entry for the listener started by
// applyShare: this binary, run against the first located target, whose
// directory is recorded in the entry. The addon's RPC port is left at its
// default (0), since it is not a configurable setting.
func listenerEntry(targets []addoninstall.Target) (autostart.Entry, error) {
	exe, err := os.Executable()
	if err != nil {
		return autostart.Entry{}, fmt.Errorf("find this program's own path: %w", err)
	}
	return autostart.Entry{
		Executable:  installExecutablePath(exe),
		UserDataDir: targets[0].UserDataDir,
		LogFile:     domain.ListenerStdoutPath(),
		FreeCADPath: resolveFreeCADPath(),
	}, nil
}

// installExecutablePath returns exe's install path when exe is itself a
// stale renamed copy left by a Windows self-update in progress: mcp-wizard's
// updater moves the running executable aside to "<target>.old-xxxxxxxx"
// before writing the new one at <target>, since a running executable can be
// renamed but not overwritten there, so `update` restarting a registered
// listener (restartListenerIfRegistered, lifecycle.go) can call
// os.Executable() while it is still the old, now-renamed process and record
// that temporary name instead of where the new binary actually lands (one
// RemoveStaleBinaries deletes on the program's next start). exe is returned
// unchanged when it does not end in ".old-" plus a suffix.
func installExecutablePath(exe string) string {
	base := filepath.Base(exe)
	if i := strings.LastIndex(base, ".old-"); i >= 0 {
		return filepath.Join(filepath.Dir(exe), base[:i])
	}
	return exe
}

// resolveFreeCADPath finds the FreeCAD GUI command to record in the
// listener's autostart entry, so a remote start_freecad works without
// depending on the listener process's own environment or PATH (an AI
// client's FREECAD_MCP_FREECAD reaches the MCP server, not a listener
// started at logon): FREECAD_MCP_FREECAD here (on this computer, while
// sharing it) when set, else the same detection start_freecad itself falls
// back to. "" when neither finds one, leaving the listener to try its own
// detection at start time.
func resolveFreeCADPath() string {
	if v := strings.TrimSpace(os.Getenv(domain.EnvFreecadGUI)); v != "" {
		if cmd, err := domain.SplitCommand(v); err == nil {
			return domain.JoinCommand(cmd)
		}
	}
	if cmd := headless.DetectGUI(context.Background()); cmd != nil {
		return domain.JoinCommand(cmd)
	}
	return ""
}

// applyUnshare turns remote access off in every target's addon settings and
// stops and unregisters the listener, reporting each step as applyShare does.
func applyUnshare(ctx context.Context, w io.Writer, targets []addoninstall.Target) int {
	if len(targets) == 0 {
		fmt.Fprintln(w, freecadNotFound)
		return 1
	}
	ok := applyUnshareSettings(w, targets, false)
	if !applyUnshareListener(w) {
		ok = false
	}
	if !ok {
		return 1
	}
	return 0
}

// applyUnshareListener stops and unregisters the listener, printing one
// [ok] or [fail] line. Shared by applyUnshare (the `share --off` command)
// and the install wizard's applyShareListenerOff (wizard_share.go) so the
// message exists once. It reports whether the listener was removed.
func applyUnshareListener(w io.Writer) bool {
	if err := autostart.Unregister(); err != nil {
		fmt.Fprintf(w, "  [fail] stop and remove the listener: %v\n", err)
		return false
	}
	fmt.Fprintln(w, "  [ok] Stopped and removed the listener")
	return true
}

// applyUnshareSettings turns remote access off in every target's addon
// settings, printing one [ok] line, or a [fail] line per target that could
// not be updated. dryRun prints what would change instead of writing
// anything. It is the settings-off twin of applyShareSettings, shared by
// applyUnshare and the install wizard. It reports whether every target
// succeeded.
func applyUnshareSettings(w io.Writer, targets []addoninstall.Target, dryRun bool) bool {
	if dryRun {
		fmt.Fprintln(w, "  would turn remote access off")
		return true
	}
	ok := true
	for _, t := range targets {
		if err := addoninstall.UpdateSettings(t, map[string]any{addoninstall.KeyRemoteEnabled: false}); err != nil {
			fmt.Fprintf(w, "  [fail] turn off remote access in %s: %v\n", addoninstall.SettingsPath(t), err)
			ok = false
		}
	}
	if ok {
		fmt.Fprintln(w, "  [ok] Remote access off")
	}
	return ok
}

// shareTestLine is one result line of a share test, such as
// "[ok] 192.168.1.23:9876 answers with the password (tested from this
// computer; test from the other device with Connect)".
type shareTestLine struct {
	Text string
}

// testShare tests opts without saving them: through a temporary in-process
// listener (listener.Options.Once) when the port is free, or the running
// listener when it holds the port, one line per LAN address.
func testShare(ctx context.Context, opts shareOptions) []shareTestLine {
	if _, err := domain.ParseAllowedIPs(opts.AllowedIPs); err != nil {
		return []shareTestLine{{Text: "[fail] " + err.Error()}}
	}

	already, probeErr := remote.Probe(ctx, remote.Endpoint{Host: "127.0.0.1", Port: opts.Port, Token: opts.Password})
	// A loopback-only allowed list (as typed on screen) means Save would
	// bind the listener to 127.0.0.1 only (domain.LoopbackOnly): the test
	// must bind and check the same way, or it would either fail every LAN
	// address for a setup that never intended to offer them, or (the
	// temporary listener path) silently widen the bind to prove
	// reachability it will not actually have after Save.
	loopbackOnly := false
	if prefixes, err := domain.ParseAllowedIPs(opts.AllowedIPs); err == nil {
		loopbackOnly = domain.LoopbackOnly(prefixes)
	}
	switch {
	case already:
		// Our own listener already holds the port: it still runs its own
		// saved settings, not the values on screen, so this computer's own
		// address may not be in its allowed list.
		lines := testListenerAddresses(ctx, opts, true, loopbackOnly)
		lines = append(lines, shareTestLine{Text: "Save applies the new settings."})
		return lines
	case errors.Is(probeErr, remote.ErrNotListener):
		return []shareTestLine{{Text: portInUseLine(opts.Port)}}
	}
	// Nothing with the listener marker answered: the port looks free. Bind a
	// temporary listener with the values on screen (this brings up the
	// firewall prompt now, unless the list is loopback-only, which never
	// needs one).
	return testTempListener(ctx, opts, loopbackOnly)
}

// testTempListener runs a temporary, in-process listener (Options.Once)
// serving opts, tests it, then shuts it down. loopbackOnly binds and tests
// 127.0.0.1 only, matching what Save would actually do for this allowed
// list; otherwise this computer's own LAN addresses are added to the
// listener's allowed list for the test only (opts.AllowedIPs as typed is
// what Save writes), so the address-by-address test can always reach it.
func testTempListener(ctx context.Context, opts shareOptions, loopbackOnly bool) []shareTestLine {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	allowedIPs := opts.AllowedIPs
	if !loopbackOnly {
		allowedIPs = addSelfTestAddresses(opts.AllowedIPs)
	}
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(runCtx, listener.Options{
			Version: version,
			Override: &addoninstall.RemoteSettings{
				RemoteEnabled:         true,
				AllowedIPs:            allowedIPs,
				AuthToken:             opts.Password,
				SessionTimeoutMinutes: opts.TimeoutMinutes,
				ListenerPort:          opts.Port,
			},
			Once:  true,
			Ready: func(string) { close(ready) },
		})
	}()

	select {
	case <-ready:
		lines := testListenerAddresses(ctx, opts, false, loopbackOnly)
		cancel()
		<-done
		return lines
	case err := <-done:
		if isAddrInUse(err) {
			return []shareTestLine{{Text: portInUseLine(opts.Port)}}
		}
		return []shareTestLine{{Text: fmt.Sprintf("[fail] could not start a test listener on port %d: %v", opts.Port, err)}}
	case <-time.After(listenerapi.StatusTimeout):
		cancel()
		<-done
		return []shareTestLine{{Text: fmt.Sprintf("[fail] the test listener on port %d did not start in time", opts.Port)}}
	}
}

// addSelfTestAddresses appends this computer's own LAN addresses to
// allowedIPs (each as a single-host entry), so a temporary test listener
// serving that list always accepts the address-by-address test that follows,
// whatever the on-screen allowed list says.
func addSelfTestAddresses(allowedIPs string) string {
	extra := lanAddresses()
	if len(extra) == 0 {
		return allowedIPs
	}
	return allowedIPs + ", " + strings.Join(extra, ", ")
}

// testListenerAddresses tests a listener on opts.Port, one line per address:
// only 127.0.0.1 when loopbackOnly (the allowed list binds nothing else),
// else every LAN address of this computer. alreadyRunning is true when the
// listener is the real, already-running one, whose allowed list is whatever
// was last saved (not opts.AllowedIPs): there, a plain connection failure
// (no response at all, as the listener's allowlist guard produces) most
// likely means this computer's address is not in that list, so it gets a
// clear explanation instead of a raw [fail] line. A refusal the listener did
// answer (a wrong password, or another reason) keeps its own message.
func testListenerAddresses(ctx context.Context, opts shareOptions, alreadyRunning, loopbackOnly bool) []shareTestLine {
	addrs := lanAddresses()
	if loopbackOnly {
		addrs = []string{"127.0.0.1"}
	}
	lines := make([]shareTestLine, 0, len(addrs))
	for _, addr := range addrs {
		_, err := remote.Status(ctx, remote.Endpoint{Host: addr, Port: opts.Port, Token: opts.Password})
		switch {
		case err == nil:
			how := "answers with the password"
			if opts.Password == "" {
				how = "answers"
			}
			lines = append(lines, shareTestLine{Text: fmt.Sprintf(
				"[ok] %s %s (tested from this computer; test from the other device with Connect)",
				hostPort(addr, opts.Port), how)})
		case alreadyRunning && !listenerAnswered(err):
			lines = append(lines, shareTestLine{Text: fmt.Sprintf(
				"[warn] %s (this computer) is not in the running listener's allowed list, so it cannot test "+
					"itself; test from the other device with Connect.", addr)})
		default:
			lines = append(lines, shareTestLine{Text: fmt.Sprintf("[fail] %s %v", hostPort(addr, opts.Port), err)})
		}
	}
	return lines
}

// listenerAnswered reports whether err came from a listener that did answer
// (a password rejection or another refusal with its own body), as opposed to
// nothing answering at all, which is what a per-address allowed-list
// rejection looks like to the caller (the connection is closed before any
// response).
func listenerAnswered(err error) bool {
	if errors.Is(err, remote.ErrPasswordRequired) || errors.Is(err, remote.ErrNotListener) {
		return true
	}
	var refusal *remote.Error
	return errors.As(err, &refusal)
}

// connectResult is the outcome of testing a connection to a listener on
// another computer: it answered, asked for a password, answered without the
// listener marker, or nothing answered at all.
type connectResult struct {
	Status           listenerapi.Status // set when a listener answered 200
	PasswordRequired bool               // a listener answered 401
	NotListener      bool               // something answered without the listener marker
	Err              error              // nothing answered (or another listener refusal)
}

// testConnect tests host:port with password.
func testConnect(ctx context.Context, host string, port int, password string) connectResult {
	ep := remote.Endpoint{Host: host, Port: port, Token: password}
	status, err := remote.Status(ctx, ep)
	switch {
	case err == nil:
		return connectResult{Status: status}
	case errors.Is(err, remote.ErrPasswordRequired):
		return connectResult{PasswordRequired: true}
	case errors.Is(err, remote.ErrNotListener):
		return connectResult{NotListener: true}
	default:
		return connectResult{Err: err}
	}
}

// saveConnection stores host, port (as a string) and password in the
// credential store; an empty password removes the stored one.
func saveConnection(ctx context.Context, host string, port int, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	sess, err := loadCredentialSession(ctx)
	if err != nil {
		return err
	}
	sess.Set(domain.HostKey, host)
	sess.Set(domain.PortKey, strconv.Itoa(port))
	setToken(sess, password)
	return saveCredentialSession(ctx, sess)
}

// clearConnection removes the stored host and port ("Use FreeCAD on this
// computer instead", `connect --clear`), restoring the credential store's
// token to this computer's own "Share this PC" password when it has one
// (whether or not remote access is currently on), else removing it too: the
// token belongs to whichever computer's FreeCAD the local agents use next.
func clearConnection(ctx context.Context) error {
	sess, err := loadCredentialSession(ctx)
	if err != nil {
		return err
	}
	delete(sess.Values, domain.HostKey)
	delete(sess.Values, domain.PortKey)
	setToken(sess, localSharePassword(ctx))
	return saveCredentialSession(ctx, sess)
}

// localSharePassword is this computer's own "Share this PC" password, when
// one is set (whether or not remote access is currently on), else "".
func localSharePassword(ctx context.Context) string {
	targets := addoninstall.LocateInstalled(ctx, freecadCommand())
	if len(targets) == 0 {
		return ""
	}
	settings, err := addoninstall.ReadRemoteSettings(targets[0])
	if err != nil {
		return ""
	}
	// The addon's own auth check applies whether or not remote access is
	// currently on (local requests are not exempt), so a password set while
	// sharing is off still belongs in the credential store, not only while
	// RemoteEnabled is true.
	return settings.AuthToken
}

// setCredentialToken sets or removes the password in the credential store,
// keeping the other stored keys (host, port).
func setCredentialToken(ctx context.Context, password string) error {
	sess, err := loadCredentialSession(ctx)
	if err != nil {
		return err
	}
	setToken(sess, password)
	return saveCredentialSession(ctx, sess)
}

func setToken(sess *secret.Session, password string) {
	if password == "" {
		delete(sess.Values, domain.TokenKey)
		return
	}
	sess.Set(domain.TokenKey, password)
}

func loadCredentialSession(ctx context.Context) (*secret.Session, error) {
	sess, _, err := secret.NewFileStore(domain.CredentialPath()).Load(ctx)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func saveCredentialSession(ctx context.Context, sess *secret.Session) error {
	return secret.NewFileStore(domain.CredentialPath()).Save(ctx, sess)
}

// lanInterfaceAddrs are this computer's private IPv4 addresses on
// up-and-running, non-loopback interfaces.
func lanInterfaceAddrs() []*net.IPNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []*net.IPNet
	for _, iface := range ifaces {
		if iface.Flags&(net.FlagUp|net.FlagRunning) != net.FlagUp|net.FlagRunning {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				// IPv4 only: IPv6 (including link-local) is skipped.
				continue
			}
			out = append(out, &net.IPNet{IP: ip4, Mask: ipNet.Mask[len(ipNet.Mask)-net.IPv4len:]})
		}
	}
	return out
}

// lanSubnets suggests the allowed IP list: the private IPv4 subnets of this
// computer's interfaces that are up and running, as CIDR strings. It is the
// fallback suggestedAllowedIPs uses when no default route is found.
func lanSubnets() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range lanInterfaceAddrs() {
		ones, bits := n.Mask.Size()
		if bits == 0 {
			continue
		}
		cidr := fmt.Sprintf("%s/%d", n.IP.Mask(n.Mask).String(), ones)
		if !seen[cidr] {
			seen[cidr] = true
			out = append(out, cidr)
		}
	}
	sort.Strings(out)
	return out
}

// defaultRouteAddress is this computer's IP address on the interface that
// would carry traffic to the public internet, found the way the OS itself
// would pick a route: connecting a UDP socket sends no packet, only asks
// the kernel to choose a source address and interface for the destination.
// "" when that fails (no route, or the lookup itself errored).
func defaultRouteAddress() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return ""
	}
	return addr.IP.String()
}

// defaultRouteSubnet is the CIDR subnet of the interface carrying
// defaultRouteAddress, or "" when that address does not match any of this
// computer's own interfaces (lanInterfaceAddrs), or there is no default
// route at all.
func defaultRouteSubnet() string {
	def := defaultRouteAddress()
	if def == "" {
		return ""
	}
	for _, n := range lanInterfaceAddrs() {
		if n.IP.String() != def {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits == 0 {
			return ""
		}
		return fmt.Sprintf("%s/%d", n.IP.Mask(n.Mask).String(), ones)
	}
	return ""
}

// suggestedAllowedIPs is the allowed-IP list to prefill a share that is not
// on yet: the default-route interface's own subnet, so a laptop on a VPN or
// with virtual adapters (WSL, Hyper-V, Docker) is not silently offered every
// private subnet it has; every private subnet (lanSubnets) is the fallback
// when no default route was found.
func suggestedAllowedIPs() []string {
	if subnet := defaultRouteSubnet(); subnet != "" {
		return []string{subnet}
	}
	return lanSubnets()
}

// lanAddresses are this computer's own LAN IP addresses (not subnets), used
// to test a listener from this computer and to print in "other devices
// connect to": the default-route address first (the one other devices are
// most likely to reach this computer at), then any others, sorted.
func lanAddresses() []string {
	seen := map[string]bool{}
	var out []string
	if def := defaultRouteAddress(); def != "" {
		out = append(out, def)
		seen[def] = true
	}
	var rest []string
	for _, n := range lanInterfaceAddrs() {
		ip := n.IP.String()
		if !seen[ip] {
			seen[ip] = true
			rest = append(rest, ip)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// shareAddressLine renders this computer's default-route address as an
// "other devices connect to" line. It once added every other private
// address any up interface had as "(or a.b.c.d:port, ...)", which on a
// machine with WSL, a VM host, or similar listed virtual adapters no
// external device could actually use (live check N3); dropped rather than
// filtered, since telling a genuine second LAN interface apart from those
// would need more than an address family and a private-range check.
func shareAddressLine(port int) string {
	addr := defaultRouteAddress()
	if addr == "" {
		return fmt.Sprintf("this computer's network address on port %d", port)
	}
	return hostPort(addr, port)
}
