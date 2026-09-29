// Package guide holds the freecad-mcp-guide skill: SKILL.md and the guide
// files it points to, embedded so the guide always matches the installed
// server. The installer writes them into AI clients' skill folders
// (skill_install.go in package main) and the asset_creation_strategy prompt
// returns the SKILL.md body.
package guide

import (
	"embed"
	"io/fs"
	"strings"
)

// Name is the skill's name and the name of its folder.
const Name = "freecad-mcp-guide"

// Generator is the frontmatter marker that says freecad-mcp wrote a copy of the
// skill: install replaces and uninstall removes only folders that carry it.
const Generator = "generator: freecad-mcp"

//go:embed freecad-mcp-guide
var files embed.FS

// FS is the skill folder: SKILL.md and the guide files at its top level.
func FS() fs.FS {
	sub, err := fs.Sub(files, Name)
	if err != nil {
		panic(err)
	}
	return sub
}

// SkillMarkdown is the full SKILL.md, frontmatter included.
func SkillMarkdown() string {
	data, err := fs.ReadFile(FS(), "SKILL.md")
	if err != nil {
		panic(err)
	}
	return string(data)
}

// splitFrontmatter splits a SKILL.md into its YAML frontmatter and its body;
// the frontmatter is empty when the text has none.
func splitFrontmatter(text string) (frontmatter, body string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if front, body, found := strings.Cut(rest, "\n---\n"); found {
			return front, strings.TrimLeft(body, "\n")
		}
	}
	return "", text
}

// Body is SKILL.md without its frontmatter.
func Body() string {
	_, body := splitFrontmatter(SkillMarkdown())
	return body
}

// Full is the body followed by every guide file as its own headed section, so
// the text stands alone where the files are not on disk (the MCP prompt).
func Full() string {
	var b strings.Builder
	b.WriteString(Body())
	entries, err := fs.ReadDir(FS(), ".")
	if err != nil {
		panic(err)
	}
	first := true
	for _, e := range entries {
		if e.IsDir() || e.Name() == "SKILL.md" {
			continue
		}
		if first {
			b.WriteString("\nThe guide files follow. A mention of a file such as values.md means its section below.\n")
			first = false
		}
		data, err := fs.ReadFile(FS(), e.Name())
		if err != nil {
			panic(err)
		}
		b.WriteString("\n# Guide file: " + e.Name() + "\n\n")
		b.WriteString(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")))
		b.WriteString("\n")
	}
	return b.String()
}

// IsOurs reports whether the SKILL.md text was written by freecad-mcp: its
// frontmatter, not its body, holds the marker as a metadata line.
func IsOurs(skillMarkdown string) bool {
	front, _ := splitFrontmatter(skillMarkdown)
	for _, line := range strings.Split(front, "\n") {
		if line == "  "+Generator {
			return true
		}
	}
	return false
}
