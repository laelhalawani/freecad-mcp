package headless

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startJob(t *testing.T, m *Jobs, code string, timeout float64) *Job {
	t.Helper()
	j, msg := m.Start(context.Background(), code, timeout, python(t))
	if msg != "" {
		t.Fatal(msg)
	}
	t.Cleanup(func() { j.Cancel() })
	return j
}

func waitDone(t *testing.T, j *Job) {
	t.Helper()
	select {
	case <-j.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not end")
	}
}

func TestJobRunningThenCancelled(t *testing.T) {
	dir := scripts(t)
	m := NewJobs()
	j := startJob(t, m, "import time\nprint('started')\ntime.sleep(60)\n", 120)
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(j.Snapshot().Output, "started") {
		if time.Now().After(deadline) {
			t.Fatal("output did not stream while the job ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s := j.Snapshot(); s.State != StateRunning || s.OutputFile == "" {
		t.Fatalf("running job reported %+v", s)
	}
	if !j.Cancel() {
		t.Fatal("cancel of a running job reported it had finished")
	}
	s := j.Snapshot()
	if s.State != StateCancelled || s.Success {
		t.Fatalf("cancelled job reported %+v", s)
	}
	assertEmpty(t, dir) // the read removed the output file, and the script is gone
}

func TestJobFinishedKeepsItsOutputForLaterReads(t *testing.T) {
	dir := scripts(t)
	m := NewJobs()
	j := startJob(t, m, "print('done')\n", 60)
	waitDone(t, j)
	first := j.Snapshot()
	if first.State != StateFinished || !first.Success || first.ExitCode == nil || *first.ExitCode != 0 || first.Output != "done" || !first.Removed {
		t.Fatalf("finished job reported %+v", first)
	}
	again := j.Snapshot()
	if again.Output != "done" || again.Removed {
		t.Fatalf("second read reported %+v", again)
	}
	assertEmpty(t, dir)
}

func TestCancelAfterTheProcessEndedReportsFinished(t *testing.T) {
	scripts(t)
	m := NewJobs()
	j := startJob(t, m, "print('before')\nraise SystemExit(3)\n", 60)
	waitDone(t, j)
	if j.Cancel() {
		t.Fatal("cancel of a finished job claimed to stop it")
	}
	s := j.Snapshot()
	if s.State != StateFinished || s.Success || s.ExitCode == nil || *s.ExitCode != 3 {
		t.Fatalf("job that ended on its own reported %+v", s)
	}
}

func TestSweepRemovesOnlyOldFinishedLogs(t *testing.T) {
	dir := scripts(t)
	old := time.Now().Add(-2 * KeepFor)
	files := map[string]bool{ // name -> kept
		"job-headless-aaaaaaaa" + finishedSuffix: false, // old and finished
		"job-headless-bbbbbbbb.log":              true,  // old but its job may still run
		"script-1.py":                            true,
	}
	for name := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, old, old)
	}
	recent := filepath.Join(dir, "job-headless-cccccccc"+finishedSuffix)
	os.WriteFile(recent, []byte("x"), 0o644)
	files[filepath.Base(recent)] = true
	NewJobs().SweepAt(nil)
	for name, kept := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != kept {
			t.Errorf("%s: kept=%v, want %v", name, err == nil, kept)
		}
	}
}
