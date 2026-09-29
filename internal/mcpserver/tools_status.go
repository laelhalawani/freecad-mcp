package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/freecad-mcp/internal/procstat"
	"github.com/sairaph/freecad-mcp/internal/winsession"
	"github.com/sairaph/mcp-wizard/render"
)

type femInput struct {
	DocName      string `json:"doc_name"`
	AnalysisName string `json:"analysis_name"`
	Timeout      *int   `json:"timeout,omitempty"`
	screenshotOptions
}

type statusFront struct {
	Freecad        string  `yaml:"freecad"`
	RPC            string  `yaml:"rpc"`
	VersionCheck   string  `yaml:"version_check,omitempty"`
	PID            *int    `yaml:"pid,omitempty"`
	ElapsedSeconds *int    `yaml:"elapsed_seconds,omitempty"`
	ExitCode       *int    `yaml:"exit_code,omitempty"`
	LogFile        *string `yaml:"log_file,omitempty"`
	ActiveDocument *string `yaml:"active_document,omitempty"`
	DocumentCount  *int    `yaml:"document_count,omitempty"`
	// Hostname is the computer FreeCAD actually runs on (its own
	// os.Hostname()), whenever the reply carries one (live check L11).
	Hostname *string `yaml:"hostname,omitempty"`
	// Desktop is "hidden" when FreeCAD runs in Windows session 0, which has no
	// desktop; left out otherwise.
	Desktop string `yaml:"desktop,omitempty"`
	// CPUCores is how many cores FreeCAD's process used over a short window
	// while its RPC server did not answer, and Busy whether that is enough to
	// call it busy computing.
	CPUCores *float64 `yaml:"cpu_cores,omitempty"`
	Busy     bool     `yaml:"busy,omitempty"`

	// The session lock fields, filled from sessionFront's fields (kept as
	// plain fields rather than an embedded sessionFields so this struct never
	// depends on how that one is declared). SessionLock is omitempty: left
	// out entirely, not "off", on the direct path when FreeCAD has never
	// answered since this process started, so whether remote access is even
	// on is not knowable at all (live-fixes review, remaining nits).
	SessionLock           string `yaml:"session_lock,omitempty"`
	SessionHolder         string `yaml:"session_holder,omitempty"`
	SessionIdleSeconds    *int   `yaml:"session_idle_seconds,omitempty"`
	SessionFreesInSeconds *int   `yaml:"session_frees_in_seconds,omitempty"`
}

type femFront struct {
	Analysis        string   `yaml:"analysis"`
	Success         bool     `yaml:"success"`
	MaxVonMisesMPa  *float64 `yaml:"max_von_mises_mpa,omitempty"`
	MaxDisplacement *float64 `yaml:"max_displacement_mm,omitempty"`
	NodeCount       *int64   `yaml:"node_count,omitempty"`
	Transaction     string   `yaml:"transaction,omitempty"`
}

// maxFEMTimeout bounds the solver wait (a week), so the reply timeout derived
// from it stays a sane duration.
const maxFEMTimeout = 7 * 24 * 3600

func (s *Server) registerStatusTools() {
	addTool(s.mcpServer, "get_rpc_status", inputSchema[struct{}](nil), s.getRPCStatus)
	addTool(s.mcpServer, "run_fem_analysis",
		withPositiveMax(inputSchema[femInput](mergeDefaults(screenshotDefaults, map[string]string{"timeout": "600"})), "timeout", maxFEMTimeout),
		s.runFEMAnalysis)
}

// getRPCStatus never errors because FreeCAD is down: it probes for FreeCAD
// itself instead of going through s.fc.get (which would turn that into an
// error result), and always builds a plain success reply, classifying what
// it found into the freecad/rpc states get_rpc_status promises. A rejection
// (bad auth token, or the caller's address not allowed) and an addon
// protocol mismatch are not "down" either way, so they keep their own,
// separate reports instead of a down state.
func (s *Server) getRPCStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	pr := s.fc.probe(ctx)
	if pr.rejected != nil {
		return s.withNotice(render.ErrorResult(*pr.rejected)), nil, nil
	}
	if pr.identityMismatch != "" {
		// Another computer answers this loopback host (live check L11):
		// refused the same way any other call is, not shown as a footnote
		// under an otherwise successful reply.
		return s.withNotice(render.ErrorResult(identityMismatchError(pr.identityMismatch))), nil, nil
	}
	if pr.oldAddon {
		return s.withNotice(render.ErrorResult(render.Error{
			Code:    codeFreeCAD,
			Message: freecad.AddonVersionWarning(nil, s.config.Version),
			Hint:    "Run `" + domain.BinaryName + " install-addon` and restart FreeCAD.",
		})), nil, nil
	}

	front := statusFront{}
	var sections []string
	// remoteEnabledDown is set when remote access is known to be on while
	// FreeCAD itself is down (a listener's own settings say so independent
	// of the addon, live check L7; or, on the direct path, the last
	// successful get_rpc_status did): session_lock is then "unknown"
	// instead of sessionFront's own "off" default, since its actual state
	// (held or free) cannot be read right now, only that it is not simply
	// off. sessionKnown is false only on the direct path with no last
	// successful reading at all (never connected since this process
	// started): session_lock is then left out of the reply entirely,
	// rather than guessing "off" (live-fixes review, remaining nits).
	remoteEnabledDown := false
	sessionKnown := true

	switch {
	case pr.fault != "":
		// FreeCAD answered and the connection still works; only this
		// get_rpc_status call raised inside the addon (a bug in
		// status_snapshot or the dispatch around it). Report FreeCAD as
		// running rather than misclassifying this as down.
		front.RPC = "reachable"
		front.Freecad = "running"
		sections = append(sections,
			"FreeCAD answered, but get_rpc_status itself raised an error in the addon:", textBlock(pr.fault))
	case pr.reachable:
		front.RPC = "reachable"
		front.Freecad = "running"
		check := "ok"
		if w := freecad.AddonVersionWarning(pr.status, s.config.Version); w != "" {
			check = w
		}
		if pr.status != nil {
			pr.status["version_check"] = check
		}
		front.VersionCheck = check
		if pid, ok := intFromStatus(pr.status, "pid"); ok {
			front.PID = &pid
		}
		if active, ok := pr.status["active_document"].(string); ok && active != "" {
			front.ActiveDocument = &active
		}
		if docs, ok := pr.status["documents"].([]any); ok {
			n := len(docs)
			front.DocumentCount = &n
		}
		hostname, _ := pr.status["hostname"].(string)
		if hostname != "" {
			front.Hostname = &hostname
		}
		summary := "FreeCAD is running and its RPC server answered."
		if hostname != "" {
			summary = fmt.Sprintf("FreeCAD is running on %s and its RPC server answered.", hostname)
		}
		sections = append(sections, summary)
		if s.hiddenDesktop(front.PID) {
			front.Desktop = "hidden"
			sections = append(sections, "FreeCAD runs in a hidden Windows session (started from an SSH or service "+
				"session), so the user cannot see it. Next step: save; ask the user to start FreeCAD on their "+
				"desktop, or to turn on sharing (`"+domain.BinaryName+" share --on`); then close_freecad when it "+
				"is listed, and start_freecad.")
		}
		sections = append(sections, jsonBlock(pr.status))
	default:
		host := s.config.FreeCAD.Host
		if s.fc.isListener(ctx) {
			// This server never launched FreeCAD itself here: the listener on
			// host did, so its own launch state (not the local launcher, which
			// knows nothing about it) is what down-state and the log tail come
			// from.
			var ls freecad.LaunchState
			if lst, lerr := s.fc.listenerStatus(ctx); lerr == nil {
				ls = lst.FreeCAD.LaunchState(time.Now())
				everAnswered := s.fc.everAnswered(ls)
				fillDownState(&front, ls, false, everAnswered)
				remoteEnabledDown = lst.RemoteEnabled
				sections = append(sections, nextStepForRemote(front.Freecad, ls, host, everAnswered))
				if lst.FreeCAD.LogTail != "" {
					sections = append(sections, fmt.Sprintf("Last launch log (`%s` on %s):", ls.LogPath, host), textBlock(lst.FreeCAD.LogTail))
				}
			} else {
				front.Freecad = "not_running"
				front.RPC = "unreachable"
				sections = append(sections,
					fmt.Sprintf("The freecad-mcp listener on %s could not be reached (%v).", host, lerr),
					fmt.Sprintf("Check that \"Share this PC\" is set up and its listener runs on %s, and that this "+
						"computer's IP address is in its allowed list; run `%s doctor` to check the setup.",
						host, domain.BinaryName))
			}
			if docs, active, ok := s.fc.lastKnownDocuments(ls); ok {
				sections = append(sections, lastKnownDocumentsBlock(docs, active, s.fc.lastStatusAge()))
			}
		} else {
			ls := s.launcher.State()
			everAnswered := s.fc.everAnswered(ls)
			fillDownState(&front, ls, pr.timedOut, everAnswered)
			if enabled, known := s.fc.lastKnownRemoteEnabled(); known {
				remoteEnabledDown = enabled
			} else {
				sessionKnown = false
			}
			next := nextStepFor(front.Freecad, ls, host, everAnswered)
			// An unresponsive FreeCAD that is using its CPU is computing, not
			// waiting behind a dialog: say so, and advise waiting.
			if front.Freecad == "unresponsive" {
				if pid := s.fc.knownPID(ls); pid > 0 {
					if cores, err := procstat.Cores(ctx, pid, procstat.Window); err == nil {
						rounded := math.Round(cores*10) / 10
						front.CPUCores = &rounded
						if cores >= procstat.BusyCores {
							front.Busy = true
							next = busyNextStep(rounded)
						}
					}
				}
			}
			sections = append(sections, next)
			if ls.LogPath != "" {
				if tail := s.launcher.LogTail(4 << 10); tail != "" {
					sections = append(sections, fmt.Sprintf("Last launch log (`%s`):", ls.LogPath), textBlock(tail))
				}
			}
			// Documents go in the body only, clearly labelled as a past reading:
			// active_document and document_count in the front matter would read
			// as current state, which they are not while freecad is not running.
			// lastKnownDocuments itself compares when that reading was taken
			// against ls.StartedAt, so a new launch never shows a previous
			// process's documents as this one's.
			if docs, active, ok := s.fc.lastKnownDocuments(ls); ok {
				sections = append(sections, lastKnownDocumentsBlock(docs, active, s.fc.lastStatusAge()))
			}
		}
	}

	// The session lock fields and sentence: always filled (sessionFront falls
	// back to the lock reading as off when status carries no session key, for
	// example while FreeCAD is down), the sentence appended only next to the
	// reachable report it explains.
	sf, sessionText := sessionFront(ctx, pr.status)
	front.SessionLock = sf.SessionLock
	front.SessionHolder = sf.SessionHolder
	front.SessionIdleSeconds = sf.SessionIdleSeconds
	front.SessionFreesInSeconds = sf.SessionFreesInSeconds
	switch {
	case remoteEnabledDown:
		// sessionFront read no session key (FreeCAD is down) and so called
		// it "off"; the listener's own settings, or the last successful
		// reading on the direct path, say otherwise, just not what state it
		// is actually in right now.
		front.SessionLock = "unknown"
	case !sessionKnown:
		// The direct path, and FreeCAD has never answered since this
		// process started: there is no listener to ask instead and no last
		// reading to fall back on, so whether remote access is even on is
		// not knowable at all. Left out of the reply (SessionLock is
		// omitempty) rather than guessing "off".
		front.SessionLock = ""
	}
	if pr.reachable && pr.fault == "" && sessionText != "" {
		sections = append(sections, sessionText)
	}

	return s.withNotice(render.SuccessResult(front, strings.Join(sections, "\n\n"))), nil, nil
}

// hiddenDesktop reports whether the FreeCAD with process id pid runs in
// Windows session 0, which has no desktop. It is false when FreeCAD is not on
// this computer, when its pid is unknown or when the session cannot be read:
// never a guess.
func (s *Server) hiddenDesktop(pid *int) bool {
	if pid == nil || s.sessionOf == nil || !domain.IsLoopbackHost(s.config.FreeCAD.Host) {
		return false
	}
	id, err := s.sessionOf(*pid)
	return err == nil && winsession.Hidden(id)
}

// fillDownState classifies why FreeCAD is unreachable (starting, unresponsive,
// exited or not_running) and fills the launch-derived fields. everAnswered
// (connector.everAnswered: a connection established for the current launch)
// rules out "starting": a launch whose RPC already answered at least once
// cannot still be starting,
// whatever the elapsed time, so it falls to unresponsive (started) or
// not_running (forwarded) instead. A forwarded launch is treated specially
// either way: its own process already exited on purpose right after handing
// the request to an already-open FreeCAD (launch_state.go), so nothing of
// this server's launch is "alive" to call unresponsive; not_running fits
// better, and its exited PID is never reported (a forwarded launch's pid
// would name a process that no longer exists, since start_freecad itself
// omits it for the same reason).
func fillDownState(front *statusFront, ls freecad.LaunchState, timedOut, everAnswered bool) {
	starting := !everAnswered && (ls.State == freecad.LaunchStarted || ls.State == freecad.LaunchForwarded) &&
		time.Since(ls.StartedAt) < freecad.StartingWindow
	switch {
	case timedOut:
		// The port accepted the connection but nothing answered in time:
		// FreeCAD is up but its RPC server (or the process itself) is stuck.
		front.Freecad = "unresponsive"
	case starting:
		front.Freecad = "starting"
	case ls.State == freecad.LaunchStarted:
		front.Freecad = "unresponsive"
	case ls.State == freecad.LaunchForwarded:
		front.Freecad = "not_running"
	case ls.State == freecad.LaunchExited:
		front.Freecad = "exited"
	default:
		front.Freecad = "not_running"
	}
	front.RPC = "unreachable"
	if ls.State == "" {
		return
	}
	if ls.State != freecad.LaunchForwarded {
		pid := ls.PID
		front.PID = &pid
	}
	elapsed := int(time.Since(ls.StartedAt).Round(time.Second).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	front.ElapsedSeconds = &elapsed
	if ls.ExitCode != nil {
		front.ExitCode = ls.ExitCode
	}
	if ls.LogPath != "" {
		front.LogFile = &ls.LogPath
	}
}

// busyNextStep is the next step for an unresponsive FreeCAD whose process is
// using its CPU (about cores cores): a long operation holds its GUI thread.
func busyNextStep(cores float64) string {
	return fmt.Sprintf("FreeCAD is busy computing (about %.1f cores of CPU over %d s): a long operation holds its GUI "+
		"thread, so its RPC server cannot answer. Wait and call get_rpc_status with {} again every 10 to 30 seconds; "+
		"send no other call meanwhile. If it stays busy far longer than the operation should take, close FreeCAD "+
		"from its window or the task manager, then call start_freecad with {}.", cores, int(procstat.Window/time.Second))
}

// nextStepFor states what to do next for a freecad state get_rpc_status
// reported while FreeCAD was unreachable. host is this server's configured
// FreeCAD host (config.FreeCAD.Host): not_running only suggests start_freecad
// when it could actually start FreeCAD here (refuseNonLoopbackHost agrees);
// for a remote host it says where to start FreeCAD instead, since
// start_freecad would only refuse. ls tells not_running apart from a
// forwarded launch whose target went away, which gets its own next step,
// worded differently depending on everAnswered: whether that other FreeCAD
// ever actually answered before it (or its RPC server) went away, or never
// answered at all.
func nextStepFor(state string, ls freecad.LaunchState, host string, everAnswered bool) string {
	switch state {
	case "starting":
		return "FreeCAD is starting. Call get_rpc_status with {} again in a few seconds; it usually reaches " +
			"rpc: reachable within 15 to 30 seconds of start_freecad."
	case "unresponsive":
		return "FreeCAD is running but its RPC server has not answered for a while. Check the FreeCAD window " +
			"for a dialog blocking it (a save prompt, an importer's options dialog) and close it, then call " +
			"get_rpc_status again; if it stays unresponsive, close FreeCAD and call start_freecad again. Run `" +
			domain.BinaryName + " doctor` to check the setup."
	case "exited":
		return "The FreeCAD process this server launched has exited. Call start_freecad with {} to launch it again."
	default: // not_running
		if ls.State == freecad.LaunchForwarded {
			if everAnswered {
				return "start_freecad forwarded this to a FreeCAD window that was already open; its RPC server " +
					"did answer for a while but has since stopped, most likely because that window was closed. " +
					"Call start_freecad with {} to launch a fresh one."
			}
			return "start_freecad forwarded this to a FreeCAD window that was already open, but its RPC server " +
				"never answered (it may have closed, or its addon is bound to a different port than " +
				domain.EnvPort + " configures). Check that window's Report View, then call start_freecad with {} again."
		}
		if refuseNonLoopbackHost(host) != nil {
			return fmt.Sprintf("FreeCAD is not running, and this server is configured for FreeCAD on %s: start "+
				"it there, then call get_rpc_status with {} to check on it.", host)
		}
		return "FreeCAD is not running. Call start_freecad with {} to launch it, then get_rpc_status with {} " +
			"to check on it."
	}
}

// nextStepForRemote is nextStepFor for FreeCAD behind a listener on host: it
// names host throughout, and, unlike nextStepFor, never tells the caller to
// start FreeCAD there themselves, since start_freecad reaches it through the
// listener.
func nextStepForRemote(state string, ls freecad.LaunchState, host string, everAnswered bool) string {
	switch state {
	case "starting":
		return fmt.Sprintf("FreeCAD is starting on %s. Call get_rpc_status with {} again in a few seconds; it "+
			"usually reaches rpc: reachable within 15 to 30 seconds of start_freecad.", host)
	case "unresponsive":
		return fmt.Sprintf("FreeCAD is running on %s but its RPC server has not answered for a while. Check the "+
			"FreeCAD window there for a dialog blocking it (a save prompt, an importer's options dialog); if it "+
			"stays unresponsive, close FreeCAD on %s and call start_freecad again. Run `%s doctor` to check the setup.",
			host, host, domain.BinaryName)
	case "exited":
		return fmt.Sprintf("The FreeCAD process on %s has exited. Call start_freecad with {} to launch it again there.", host)
	default: // not_running
		if ls.State == freecad.LaunchForwarded {
			if everAnswered {
				return fmt.Sprintf("start_freecad forwarded this to a FreeCAD window already open on %s; its RPC "+
					"server did answer for a while but has since stopped, most likely because that window was "+
					"closed. Call start_freecad with {} to launch a fresh one there.", host)
			}
			return fmt.Sprintf("start_freecad forwarded this to a FreeCAD window already open on %s, but its RPC "+
				"server never answered. Check that window's Report View on %s, then call start_freecad with {} again.",
				host, host)
		}
		return fmt.Sprintf("FreeCAD is not running on %s. Call start_freecad with {} to launch it there, then "+
			"get_rpc_status with {} to check on it.", host)
	}
}

// lastKnownDocumentsBlock renders the documents get_rpc_status last saw while
// FreeCAD was reachable, for a reply made while it is down.
func lastKnownDocumentsBlock(docs []map[string]any, active string, age time.Duration) string {
	if len(docs) == 0 {
		return fmt.Sprintf("Last known state (%s ago, before FreeCAD stopped answering): no document was open.", age.Round(time.Second))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last known open documents (%s ago, the last time FreeCAD answered or a document was created, opened, closed, imported or saved as; may be stale now):\n\n",
		age.Round(time.Second))
	b.WriteString("| document | label | file | active |\n|---|---|---|---|\n")
	for _, d := range docs {
		name, _ := d["name"].(string)
		label, _ := d["label"].(string)
		file, _ := d["file_name"].(string)
		mark := ""
		if name != "" && name == active {
			mark = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", name, label, file, mark)
	}
	return b.String()
}

// femFailureHint returns the hint for a failed analysis. The addon's GUI
// dispatch words its own failures: the analysis ran past its timeout ("timed
// out after"), did not start before its queue budget ran out ("gave up
// after"), or never started because an earlier GUI operation is still stuck
// ("unavailable"). larger says how to allow more time.
func femFailureHint(res map[string]any, larger string) string {
	e := str(res, "error")
	switch {
	case strings.HasPrefix(e, "GUI dispatch timed out after"):
		return "The analysis did not finish within its timeout. Call get_rpc_status to see whether it is still " +
			"running on FreeCAD's GUI thread. " + larger
	case strings.HasPrefix(e, "GUI dispatch gave up after"):
		return "The analysis did not start in time: earlier GUI operations held FreeCAD's GUI thread for the whole " +
			"timeout, which also bounds the wait to start. Call get_rpc_status to see what is running. A larger " +
			"timeout also allows a longer wait to start; the most is " + fmt.Sprint(maxFEMTimeout) + " seconds."
	case strings.HasPrefix(e, "GUI dispatch unavailable"):
		return "The analysis did not start: another GUI operation timed out and is still running on FreeCAD's GUI " +
			"thread. Call get_rpc_status and wait until it reports the dispatch healthy, then retry, or restart FreeCAD."
	}
	return "Check the prerequisites listed in this tool's description with list_objects; the details that follow include the working directory."
}

// femLoadsText lists each force and pressure of the analysis with its
// magnitude and the direction it acts in, or "" when the reply has none.
func femLoadsText(res map[string]any) string {
	loads, _ := res["loads"].([]any)
	var b strings.Builder
	for _, item := range loads {
		if load, ok := item.(map[string]any); ok {
			fmt.Fprintf(&b, "\n- %s: %s", str(load, "name"), str(load, "text"))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nLoads:" + b.String()
}

// femColourText says the screenshot is coloured by von Mises stress and over
// what range, or "" when the addon did not colour the result.
func femColourText(res map[string]any) string {
	if !boolField(res, "coloured") {
		return ""
	}
	rng, _ := res["colour_range_MPa"].([]any)
	if len(rng) != 2 {
		return ""
	}
	text := fmt.Sprintf("\n\nThe view colours the %s from %s to %s. The colour bar is labelled in Pa (1e6 Pa = 1 MPa).",
		str(res, "colour_field"), formatMeasure(rng[0], "MPa"), formatMeasure(rng[1], "MPa"))
	if hidden := stringItems(res["hidden_objects"]); len(hidden) > 0 {
		text += fmt.Sprintf(" %s hidden so the result shows; show one again with update_object and "+
			"{\"ViewObject\": {\"Visibility\": true}}. A shown solid covers the stress colours: hide it again, or use get_view to look.",
			strings.Join(hidden, ", "))
	}
	return text
}

func (s *Server) runFEMAnalysis(ctx context.Context, _ *mcp.CallToolRequest, in femInput) (*mcp.CallToolResult, any, error) {
	timeout := 600
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	if timeout <= 0 || timeout > maxFEMTimeout {
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: fmt.Sprintf("timeout must be a positive number of seconds, at most %d", maxFEMTimeout),
			Hint:    "Omit timeout for the 600 s default.",
		}), nil, nil
	}
	conn, err := s.fc.get(ctx)
	if err != nil {
		return failure(ctx, "run FEM analysis", err, ""), nil, nil
	}
	larger := fmt.Sprintf("For a long analysis, call run_fem_analysis again with a larger timeout (this run allowed %d seconds, at most %d).",
		timeout, maxFEMTimeout)
	res, err := conn.RunFEMAnalysis(ctx, in.DocName, in.AnalysisName, timeout)
	if err != nil {
		return s.withNotice(timedFailure(ctx, "run FEM analysis", err, larger)), nil, nil
	}
	if !succeeded(res) {
		// fem_executor's own not_found/invalid_input checks (an unknown
		// document or analysis, the wrong TypeId) carry a code and hint,
		// which win here same as everywhere else regardless of hintCodes
		// (reportedCode always prefers the addon's own hint when it sent
		// one). Everything else fem_executor returns on failure (a
		// prerequisite check, a solver run, a raised exception) carries no
		// code at all, landing on codeFreeCAD. A GUI-dispatch timing problem
		// is the same uncoded codeFreeCAD (gave up waiting, timed out) or
		// GUI_DISPATCH_STUCK, which reportedCode maps to render.CodeUnavailable.
		// femFailureHint's wording covers all of these; name both codes here
		// so it is not silently replaced by codeHints' generic default.
		out := reportedCode("run FEM analysis", res, femFailureHint(res, larger), codeFreeCAD, render.CodeUnavailable)
		out.Content = append(out.Content, &mcp.TextContent{Text: jsonBlock(res)})
		return s.withNotice(out), nil, nil
	}
	txName, txMerged := transactionFields(res)
	front := femFront{Analysis: in.AnalysisName, Success: true, Transaction: txName}
	if f, ok := number(res["max_von_mises_MPa"]); ok {
		front.MaxVonMisesMPa = &f
	}
	if f, ok := number(res["max_displacement_mm"]); ok {
		front.MaxDisplacement = &f
	}
	if n, ok := res["node_count"].(int64); ok {
		front.NodeCount = &n
	}
	res["summary"] = fmt.Sprintf("FEM analysis '%s' solved. max von Mises = %s, max displacement = %s (%v nodes).",
		in.AnalysisName, formatMeasure(res["max_von_mises_MPa"], "MPa"), formatMeasure(res["max_displacement_mm"], "mm"), res["node_count"])
	body := transactionNote(res["summary"].(string)+femLoadsText(res)+femColourText(res)+"\n\n"+jsonBlock(res), txName, txMerged)
	out := render.SuccessResult(front, body)
	return s.withNotice(s.screenshot(ctx, conn, out, in.IncludeScreenshot, viewString(in.ViewName), in.DocName)), nil, nil
}
