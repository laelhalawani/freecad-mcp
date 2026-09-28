//go:build windows

package autostart

// Windows: a Task Scheduler logon task for the current user (WindowsTaskName),
// created with schtasks from a UTF-16LE task XML. The task runs only while
// this user is logged on (LogonType InteractiveToken), needs no admin rights
// and no stored password, and is restarted by Task Scheduler itself after a
// crash (RestartOnFailure). Stopping always goes through schtasks /end, never
// by killing the process, so a FreeCAD the listener launched is never taken
// down with it.

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
)

// Structs below mirror the Task Scheduler XML schema closely enough to build
// a valid task definition; every complex type it uses (taskType, settingsType,
// principalType, execType) declares its child elements with xs:all, so their
// order in the document does not matter to the schema.

type taskXML struct {
	XMLName          xml.Name        `xml:"http://schemas.microsoft.com/windows/2004/02/mit/task Task"`
	RegistrationInfo xmlRegistration `xml:"RegistrationInfo"`
	Triggers         xmlTriggers     `xml:"Triggers"`
	Principals       xmlPrincipals   `xml:"Principals"`
	Settings         xmlSettings     `xml:"Settings"`
	Actions          xmlActions      `xml:"Actions"`
}

type xmlRegistration struct {
	Description string `xml:"Description"`
}

type xmlTriggers struct {
	LogonTrigger xmlLogonTrigger `xml:"LogonTrigger"`
}

type xmlLogonTrigger struct {
	Enabled bool   `xml:"Enabled"`
	UserId  string `xml:"UserId"`
}

type xmlPrincipals struct {
	Principal xmlPrincipal `xml:"Principal"`
}

type xmlPrincipal struct {
	Id        string `xml:"id,attr"`
	UserId    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type xmlSettings struct {
	MultipleInstancesPolicy    string     `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool       `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool       `xml:"StopIfGoingOnBatteries"`
	ExecutionTimeLimit         string     `xml:"ExecutionTimeLimit"`
	Priority                   int        `xml:"Priority"`
	RestartOnFailure           xmlRestart `xml:"RestartOnFailure"`
}

type xmlRestart struct {
	Interval string `xml:"Interval"`
	Count    int    `xml:"Count"`
}

type xmlActions struct {
	Context string  `xml:"Context,attr"`
	Exec    xmlExec `xml:"Exec"`
}

type xmlExec struct {
	Command   string `xml:"Command"`
	Arguments string `xml:"Arguments"`
}

// taskArguments builds the "listen --user-data-dir ..." command line run by
// the task. Each argument goes through syscall.EscapeArg, the same quoting
// CreateProcess and CommandLineToArgvW use, so a UserDataDir ending in a
// backslash (such as a drive root, "C:\") still closes its quote correctly
// instead of escaping it and swallowing the rest of the line. RPCPort is
// added only when it differs from the addon default, matching Entry's
// documented convention; FreeCADPath empty omits --freecad.
func taskArguments(e Entry) string {
	args := []string{"listen", "--user-data-dir", e.UserDataDir}
	if e.RPCPort != 0 && e.RPCPort != domain.DefaultRPCPort {
		args = append(args, "--rpc-port", strconv.Itoa(e.RPCPort))
	}
	if e.FreeCADPath != "" {
		args = append(args, "--freecad", e.FreeCADPath)
	}
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = syscall.EscapeArg(a)
	}
	return strings.Join(escaped, " ")
}

// taskDefinitionXML renders e as a Task Scheduler task definition for
// userID (the "DOMAIN\user" form os/user.Current returns on Windows), UTF-16LE
// encoded with a byte order mark, as schtasks /xml requires.
func taskDefinitionXML(e Entry, userID string) ([]byte, error) {
	task := taskXML{
		RegistrationInfo: xmlRegistration{
			Description: "Starts the freecad-mcp listener at logon, so FreeCAD on another computer can reach this one.",
		},
		Triggers: xmlTriggers{
			LogonTrigger: xmlLogonTrigger{Enabled: true, UserId: userID},
		},
		Principals: xmlPrincipals{
			Principal: xmlPrincipal{
				Id:        "Author",
				UserId:    userID,
				LogonType: "InteractiveToken",
				RunLevel:  "LeastPrivilege",
			},
		},
		Settings: xmlSettings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			ExecutionTimeLimit:         "PT0S",
			// 4 is Task Scheduler's "normal" priority; the default (no
			// element) is 7, below normal, which the listener and the
			// FreeCAD it launches would otherwise run at (live check L2).
			Priority:         4,
			RestartOnFailure: xmlRestart{Interval: "PT1M", Count: 3},
		},
		Actions: xmlActions{
			Context: "Author",
			Exec: xmlExec{
				Command:   e.Executable,
				Arguments: taskArguments(e),
			},
		},
	}
	body, err := xml.MarshalIndent(task, "", "  ")
	if err != nil {
		return nil, err
	}
	doc := "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n" + string(body)
	return utf16LEWithBOM(doc), nil
}

// utf16LEWithBOM encodes s as UTF-16LE with a leading byte order mark, the
// form schtasks /xml requires so non-ASCII characters in a user name or path
// survive.
func utf16LEWithBOM(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 2+2*len(units))
	buf[0], buf[1] = 0xFF, 0xFE
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2+2*i:], u)
	}
	return buf
}

// currentUserID returns the current user as Task Scheduler expects it,
// "DOMAIN\user", which os/user.Current already reports in that form on
// Windows.
func currentUserID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("looking up the current user: %w", err)
	}
	return u.Username, nil
}

// runSchtasks runs schtasks with args and returns an error carrying its full
// combined output when it fails, so callers never have to parse localised
// prose to explain a failure.
func runSchtasks(args ...string) error {
	out, err := exec.Command("schtasks", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// taskRegistered reports whether WindowsTaskName exists, from schtasks
// /query's exit code alone: 0 when the task is found, non-zero otherwise,
// regardless of the Windows display language.
func taskRegistered() bool {
	err := exec.Command("schtasks", "/query", "/tn", WindowsTaskName, "/fo", "CSV", "/nh").Run()
	return err == nil
}

func register(e Entry) error {
	userID, err := currentUserID()
	if err != nil {
		return err
	}
	data, err := taskDefinitionXML(e, userID)
	if err != nil {
		return fmt.Errorf("building the task definition: %w", err)
	}
	tmp, err := os.CreateTemp("", "freecad-mcp-listener-task-*.xml")
	if err != nil {
		return fmt.Errorf("writing the task definition: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		return fmt.Errorf("writing the task definition: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("writing the task definition: %w", closeErr)
	}
	// /f replaces an existing task of the same name, which makes Register
	// idempotent.
	return runSchtasks("/create", "/tn", WindowsTaskName, "/xml", tmpPath, "/f")
}

// unregister ends the task's running instance before deleting the task
// itself: /delete alone removes the task definition but, unverified,
// may not end an instance schtasks /run already started, which
// would otherwise keep the old listener holding its port and log files
// after "Share this PC" is turned off. /end is never reported: ending
// something not currently running is expected, not a failure. After
// deleting, this waits briefly for listenerapi.Running() to turn false, so
// a caller that immediately reports success is not lying while the process
// is still exiting.
func unregister() error {
	if !taskRegistered() {
		return nil
	}
	runSchtasks("/end", "/tn", WindowsTaskName)
	if err := runSchtasks("/delete", "/tn", WindowsTaskName, "/f"); err != nil {
		return err
	}
	waitUntilNotRunning(unregisterStopWait)
	return nil
}

// unregisterStopWait bounds how long unregister waits for the ended task's
// process to actually exit and release the listener lock.
const unregisterStopWait = 5 * time.Second

// waitUntilNotRunning polls listenerapi.Running() until it is false or
// timeout passes, whichever comes first.
func waitUntilNotRunning(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for listenerapi.Running() {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func start() error {
	return runSchtasks("/run", "/tn", WindowsTaskName)
}

// stop ends the program the task started (never Kill, so a FreeCAD it
// launched keeps running); it never deletes or disables the task itself.
func stop() error {
	return runSchtasks("/end", "/tn", WindowsTaskName)
}

// restart ends then runs the task again, so it picks up the binary the entry
// now names. /end fails harmlessly when nothing is currently running; that
// error is not reported, only /run's is.
func restart() error {
	_ = runSchtasks("/end", "/tn", WindowsTaskName)
	return runSchtasks("/run", "/tn", WindowsTaskName)
}

func status() (State, error) {
	registered := taskRegistered()
	st := State{
		Registered: registered,
		// Running always comes from the lock file, the same test every OS
		// uses, never from schtasks output.
		Running: listenerapi.Running(),
	}
	switch {
	case !registered:
		st.Detail = "Task Scheduler task not registered"
	case st.Running:
		st.Detail = "Task Scheduler task registered, listener running"
	default:
		st.Detail = "Task Scheduler task registered, listener not running"
	}
	return st, nil
}
