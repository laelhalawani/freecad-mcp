package headless

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TailLines is how many of the last output lines a job reports.
const TailLines = 200

// KeepFor is how long a finished job and its output file are kept when nobody
// reads them.
const KeepFor = 24 * time.Hour

// tailBytes bounds how much of an output file is read to find its last lines.
const tailBytes = 4 << 20

// Job states.
const (
	StateRunning   = "running"
	StateFinished  = "finished"
	StateCancelled = "cancelled"
)

// Job is one headless run in the background. Its stdout and stderr stream into
// one output file while it runs.
type Job struct {
	ID      string
	Started time.Time
	Timeout float64

	script string // the temporary script written for this job; "" for a file the caller named
	output string
	stop   context.CancelFunc
	kill   func() // ends the process and everything it started
	exited atomic.Bool
	done   chan struct{}

	mu        sync.Mutex
	finished  time.Time
	exitCode  *int
	success   bool
	crashed   bool
	timedOut  bool
	cancelled bool
	errText   string
	read      bool   // the final output was handed out and the file removed
	finalTail string // that output, kept for later reads
}

// Snapshot is what a job reports about itself.
type Snapshot struct {
	ID         string
	State      string
	Elapsed    time.Duration
	Timeout    float64
	ExitCode   *int
	Success    bool
	Crashed    bool
	TimedOut   bool
	Error      string
	Output     string // the last TailLines lines
	OutputFile string // "" once removed
	Removed    bool   // the output file was removed by this read
}

// Jobs runs and tracks background headless jobs.
type Jobs struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewJobs returns an empty job list.
func NewJobs() *Jobs { return &Jobs{jobs: map[string]*Job{}} }

// IsJobID reports whether id looks like a headless job id.
func IsJobID(id string) bool { return strings.HasPrefix(id, "headless-") }

func newJobID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "headless-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "headless-" + hex.EncodeToString(b)
}

// Start begins code in the background and returns at once. ctx only supplies
// the freecadcmd detection; the job outlives the request and runs until it
// ends, timeout seconds pass, Cancel or StopAll. msg is the error text when it
// could not start.
func (m *Jobs) Start(ctx context.Context, code string, timeout float64, command []string) (job *Job, msg string) {
	return m.StartScript(ctx, Script{Code: code}, timeout, command)
}

// StartScript is Start for a Script: inline code or a file that already
// exists, which the job never removes.
func (m *Jobs) StartScript(ctx context.Context, s Script, timeout float64, command []string) (job *Job, msg string) {
	command, dir, msg := prepare(ctx, timeout, command)
	if msg != "" {
		return nil, msg
	}
	m.sweep(dir)
	script, temp, msg := s.prepare(dir)
	if msg != "" {
		return nil, msg
	}
	// removeTemp removes the script written for this job; a file the caller
	// named is left alone.
	removeTemp := func() {
		if temp {
			os.Remove(script)
		}
	}
	id := newJobID()
	outPath := filepath.Join(dir, "job-"+id+".log")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		removeTemp()
		return nil, fmt.Sprintf("could not write the script: %v", err)
	}
	runCtx, stop := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	args := append(append([]string{}, command[1:]...), "-c", bootstrap(script))
	cmd := exec.CommandContext(runCtx, command[0], args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	kill, err := startTree(cmd)
	if err != nil {
		stop()
		out.Close()
		removeTemp()
		os.Remove(outPath)
		return nil, fmt.Sprintf("could not start headless FreeCAD: %v", err)
	}
	j := &Job{ID: id, Started: time.Now(), Timeout: timeout, output: outPath, stop: stop, kill: kill, done: make(chan struct{})}
	if temp {
		j.script = script
	}
	m.mu.Lock()
	m.jobs[id] = j
	m.mu.Unlock()
	go j.wait(runCtx, cmd, out)
	return j, ""
}

// finishedName is the name of a job's log once the job ended.
func finishedName(running string) string {
	return strings.TrimSuffix(running, ".log") + finishedSuffix
}

// finishedSuffix ends the name of a finished job's log; sweep removes only
// those.
const finishedSuffix = ".finished.log"

// Done is closed when the job's process has ended.
func (j *Job) Done() <-chan struct{} { return j.done }

// OutputFile is the file the job streams its output to.
func (j *Job) OutputFile() string { return j.output }

// wait reaps the process and records how it ended.
func (j *Job) wait(runCtx context.Context, cmd *exec.Cmd, out *os.File) {
	runErr := cmd.Wait()
	j.exited.Store(true)
	j.kill() // whatever the script left running ends with it
	out.Close()
	if j.script != "" {
		os.Remove(j.script)
	}
	defer close(j.done)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finished = time.Now()
	defer j.stop()
	// The log gets its finished name, so a sweep never touches a running job's.
	if fin := finishedName(j.output); os.Rename(j.output, fin) == nil {
		j.output = fin
	}
	switch {
	// A process that ended by itself with exit 0 was not stopped by a cancel
	// that arrived as it ended: a killed process never exits 0.
	case j.cancelled && !(runErr == nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0):
		j.errText = "the job was cancelled"
		return
	case runErr != nil && runCtx.Err() == context.DeadlineExceeded && !j.cancelled:
		j.timedOut = true
		j.errText = fmt.Sprintf("headless FreeCAD did not finish within %s s", strconv.FormatFloat(j.Timeout, 'g', -1, 64))
		return
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) && cmd.ProcessState == nil {
		j.errText = fmt.Sprintf("headless FreeCAD failed: %v", runErr)
		return
	}
	code := cmd.ProcessState.ExitCode()
	j.exitCode = &code
	j.success = code == 0 && runErr == nil
	if signal, number, ok := crashSignal(cmd.ProcessState); ok {
		j.success, j.crashed = false, true
		if number != 0 {
			j.exitCode = &number
		}
		j.errText = crashMessage(signal, code)
		return
	}
	if !j.success {
		if m := crashReport.FindStringSubmatch(readTail(j.output, TailLines)); m != nil {
			j.crashed = true
			j.errText = fmt.Sprintf("headless FreeCAD reported a crash with %s (exit code %d; the GUI is unaffected)", m[1], code)
		} else {
			j.errText = fmt.Sprintf("script failed (exit code %d)", code)
		}
	}
}

// readTail returns the last n cleaned lines of the file at path.
func readTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > tailBytes {
		f.Seek(info.Size()-tailBytes, io.SeekStart)
	}
	data, _ := io.ReadAll(f)
	lines := strings.Split(clean(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Snapshot reports the job. The first snapshot of a finished job hands out its
// final output and removes the output file; later ones repeat that output.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{ID: j.ID, Timeout: j.Timeout}
	if j.finished.IsZero() {
		s.State = StateRunning
		s.Elapsed = time.Since(j.Started)
		s.Output = readTail(j.output, TailLines)
		s.OutputFile = j.output
		return s
	}
	s.State = StateFinished
	if j.cancelled {
		s.State = StateCancelled
	}
	s.Elapsed = j.finished.Sub(j.Started)
	s.ExitCode, s.Success, s.Crashed, s.TimedOut, s.Error = j.exitCode, j.success, j.crashed, j.timedOut, j.errText
	if !j.read {
		j.finalTail = readTail(j.output, TailLines)
		os.Remove(j.output)
		j.read = true
		s.Removed = true
		s.OutputFile = j.output
	}
	s.Output = j.finalTail
	return s
}

// Cancel stops the job's process and waits briefly for it to end. It reports
// false when the job had already finished.
func (j *Job) Cancel() bool {
	j.mu.Lock()
	if !j.finished.IsZero() {
		j.mu.Unlock()
		return false
	}
	if j.exited.Load() {
		// The process ended on its own; only its bookkeeping is left.
		j.mu.Unlock()
		<-j.done
		return false
	}
	j.cancelled = true
	j.mu.Unlock()
	j.stop()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
	}
	return true
}

// Get returns the job called id, or nil.
func (m *Jobs) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// Snapshots reports every job, newest first, without consuming any output.
func (m *Jobs) Snapshots() []Snapshot {
	m.mu.Lock()
	list := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	m.mu.Unlock()
	sort.Slice(list, func(a, b int) bool { return list[a].Started.After(list[b].Started) })
	out := make([]Snapshot, 0, len(list))
	for _, j := range list {
		j.mu.Lock()
		s := Snapshot{ID: j.ID, Timeout: j.Timeout, State: StateRunning, Elapsed: time.Since(j.Started)}
		if !j.finished.IsZero() {
			s.State = StateFinished
			if j.cancelled {
				s.State = StateCancelled
			}
			s.Elapsed = j.finished.Sub(j.Started)
			s.ExitCode, s.Success, s.Crashed, s.TimedOut = j.exitCode, j.success, j.crashed, j.timedOut
		}
		j.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// Running is the number of jobs still running.
func (m *Jobs) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		j.mu.Lock()
		if j.finished.IsZero() {
			n++
		}
		j.mu.Unlock()
	}
	return n
}

// StopAll ends every running job (the server is exiting), best effort.
func (m *Jobs) StopAll() {
	m.mu.Lock()
	list := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, j := range list {
		wg.Add(1)
		go func(j *Job) {
			defer wg.Done()
			j.Cancel()
		}(j)
	}
	wg.Wait()
}

// SweepAt removes what sweep would, for the script directory of command (the
// default one when command is empty). The server calls it at start, so logs of
// an earlier server process do not wait for the next job.
func (m *Jobs) SweepAt(command []string) {
	dir, err := ScriptDir()
	if len(command) > 0 {
		dir, err = scriptDirFor(command[0])
	}
	if err == nil {
		m.sweep(dir)
	}
}

// sweep forgets finished jobs older than KeepFor and removes finished job logs
// in dir that old, which covers jobs of an earlier server process. A log of a
// running job has no finished name and stays.
func (m *Jobs) sweep(dir string) {
	cutoff := time.Now().Add(-KeepFor)
	m.mu.Lock()
	for id, j := range m.jobs {
		j.mu.Lock()
		old := !j.finished.IsZero() && j.finished.Before(cutoff)
		j.mu.Unlock()
		if old {
			os.Remove(j.output)
			delete(m.jobs, id)
		}
	}
	m.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "job-headless-") || !strings.HasSuffix(name, finishedSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, name))
		}
	}
}
