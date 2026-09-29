package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/mcp-wizard/harness"

	"github.com/sairaph/freecad-mcp/internal/guide"
)

// skillTimes gives every file under dir an old modification time and returns
// them, so a later write shows.
func skillTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	times := map[string]time.Time{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		times[path] = old
		return os.Chtimes(path, old, old)
	})
	if err != nil || len(times) == 0 {
		t.Fatalf("skill files under %s: %v, %v", dir, times, err)
	}
	return times
}

func TestASecondGuideSkillWriteLeavesTheFilesAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	ids := []harness.ID{clientClaudeCode}
	target := filepath.Join(home, ".claude", "skills", guide.Name)

	var out bytes.Buffer
	if code := installSkills(&out, harness.Scope{}, ids, false, false); code != 0 || !strings.Contains(out.String(), "wrote the guide skill") {
		t.Fatalf("first write = %d\n%s", code, out.String())
	}
	times := skillTimes(t, target)

	out.Reset()
	if code := installSkills(&out, harness.Scope{}, ids, false, false); code != 0 ||
		!strings.Contains(out.String(), "guide skill already current: "+target) || strings.Contains(out.String(), "wrote") {
		t.Fatalf("second write = %d\n%s", code, out.String())
	}
	for path, want := range times {
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(want) {
			t.Errorf("%s was rewritten: %v, %v", path, info, err)
		}
	}
	// A dry run and a refresh say the same and write nothing either.
	out.Reset()
	installSkills(&out, harness.Scope{}, ids, true, false)
	refreshSkills(&out, false)
	if strings.Count(out.String(), "already current") != 2 || strings.Contains(out.String(), "would") || strings.Contains(out.String(), "updated") {
		t.Fatalf("dry run and refresh:\n%s", out.String())
	}
}

func TestAGuideSkillThatDiffersIsWrittenAgain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	ids := []harness.ID{clientClaudeCode}
	target := filepath.Join(home, ".claude", "skills", guide.Name)
	var out bytes.Buffer
	installSkills(&out, harness.Scope{}, ids, false, false)

	// A stale file, and an edited one: both make the copy differ, and the
	// write replaces the folder.
	stale := filepath.Join(target, "stale.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	installSkills(&out, harness.Scope{}, ids, false, false)
	if !strings.Contains(out.String(), "wrote the guide skill") {
		t.Fatalf("a folder with a stale file:\n%s", out.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale file was kept: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	installSkills(&out, harness.Scope{}, ids, false, false)
	// Edited so that it lost the generator marker: left alone, as before.
	if !strings.Contains(out.String(), "did not write") {
		t.Fatalf("an edited skill:\n%s", out.String())
	}
}
