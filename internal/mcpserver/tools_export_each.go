package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/freecad"
	"github.com/sairaph/mcp-wizard/render"
)

// exportEachFront is the front matter of an export with one file per object.
type exportEachFront struct {
	Document  string `yaml:"document"`
	Folder    string `yaml:"folder"`
	Format    string `yaml:"format"`
	FileCount int    `yaml:"file_count"`
	Failed    int    `yaml:"failed"`
	Bytes     int64  `yaml:"bytes"`
}

var (
	unsafeFileChars = regexp.MustCompile("[" + `\\/:*?"<>|` + `\x00-\x1f` + "]+")
	driveLetter     = regexp.MustCompile(`^[A-Za-z]:`)
	// reservedNames are the device names Windows refuses as file names, with
	// or without an extension.
	reservedNames = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)
)

// maxStemRunes keeps a file name well inside the 255 characters file systems
// allow, leaving room for a suffix and the extension.
const maxStemRunes = 200

// exportFormats are the extensions export_document writes.
var exportFormats = map[string]bool{
	"stl": true, "ast": true, "3mf": true, "amf": true, "obj": true, "ply": true, "off": true,
	"step": true, "stp": true, "iges": true, "igs": true, "brep": true, "brp": true,
	"glb": true, "gltf": true, "fcstd": true, "dxf": true, "svg": true,
}

// fileNameFor turns an object's label into a file name stem: characters no
// file system accepts become an underscore, a Windows device name gets a
// leading underscore, a long name is cut, and an empty result falls back to
// the object's name.
func fileNameFor(label, name string) string {
	clean := func(s string) string {
		return strings.Trim(unsafeFileChars.ReplaceAllString(s, "_"), " .")
	}
	stem := clean(label)
	if stem == "" {
		stem = clean(name)
	}
	if runes := []rune(stem); len(runes) > maxStemRunes {
		stem = strings.TrimRight(string(runes[:maxStemRunes]), " .")
	}
	if reservedNames.MatchString(stem) {
		stem = "_" + stem
	}
	return stem
}

// joinFolder appends a file name to a folder path in the folder's own style
// (backslashes for a Windows path, slashes otherwise), since the path is on the
// computer running FreeCAD, which may not be this one.
func joinFolder(folder, file string) string {
	sep := "/"
	if strings.Contains(folder, `\`) || driveLetter.MatchString(folder) {
		sep = `\`
	}
	return strings.TrimRight(folder, `/\`) + sep + file
}

// labelsOf maps object names to labels from a compact object list.
func labelsOf(objects any) map[string]string {
	labels := map[string]string{}
	list, _ := objects.([]any)
	for _, o := range list {
		if m, ok := o.(map[string]any); ok {
			labels[str(m, "name")] = str(m, "label")
		}
	}
	return labels
}

// exportEach writes every object of in.ObjectNames to its own file in the
// folder in.Path, named after its label, one addon export per object.
func (s *Server) exportEach(ctx context.Context, conn *freecad.Connection, in exportDocumentInput) *mcp.CallToolResult {
	const what = "export document"
	invalid := func(msg, hint string) *mcp.CallToolResult {
		return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: msg, Hint: hint})
	}
	if len(in.ObjectNames) == 0 {
		return invalid("per_object needs object_names.", "List the objects to write, one file each, then call export_document again.")
	}
	ext := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(deref(in.Format)), "."))
	if !exportFormats[ext] {
		return invalid(fmt.Sprintf("per_object needs format to be one of the export formats, not %q.", deref(in.Format)),
			"Pass the extension of every file, for example stl, 3mf or step.")
	}
	objects, err := conn.GetObjects(ctx, in.DocName, true)
	if err != nil {
		return timedFailure(ctx, what, err, largerTimeout("export_document"))
	}
	labels := labelsOf(objects)

	front := exportEachFront{Document: in.DocName, Folder: in.Path, Format: ext}
	var body, failed strings.Builder
	used := map[string]bool{}
	var firstFailure map[string]any
	created := ""
	for _, name := range in.ObjectNames {
		stem := fileNameFor(labels[name], name)
		file := stem + "." + ext
		for n := 2; used[strings.ToLower(file)]; n++ {
			file = fmt.Sprintf("%s_%d.%s", stem, n, ext)
		}
		used[strings.ToLower(file)] = true

		single := in
		single.ObjectNames = []string{name}
		single.PerObject, single.Format = nil, nil
		res, err := conn.ExportDocument(ctx, in.DocName, joinFolder(in.Path, file), exportOptionsMap(single), in.Timeout)
		if err != nil {
			if front.FileCount > 0 {
				err = fmt.Errorf("%w (already written: %d file(s) in '%s')", err, front.FileCount, in.Path)
			}
			return timedFailure(ctx, what, err, largerTimeout("export_document"))
		}
		if note := createdFolderNote(res); note != "" && created == "" {
			created = note
		}
		if !succeeded(res) {
			if firstFailure == nil {
				firstFailure = res
			}
			front.Failed++
			fmt.Fprintf(&failed, "\n- %s: %s", name, str(res, "error"))
			continue
		}
		size := bigIntField(res, "bytes")
		front.FileCount++
		front.Bytes += size
		fmt.Fprintf(&body, "\n- %s -> %s (%d bytes)", name, str(res, "file_name"), size)
		if skipped, _ := res["skipped"].([]any); len(skipped) > 0 {
			for _, item := range skipped {
				if m, ok := item.(map[string]any); ok {
					fmt.Fprintf(&body, "; skipped %s: %s", str(m, "name"), str(m, "reason"))
				}
			}
		}
	}
	if front.FileCount == 0 && firstFailure != nil {
		return reportedCode(what, firstFailure,
			fmt.Sprintf("Call list_objects with {\"doc_name\": %q} to see the document's objects.", in.DocName))
	}
	text := fmt.Sprintf("Exported %d of %d object(s) from '%s' as %s files into '%s', one file each.%s\n%s",
		front.FileCount, len(in.ObjectNames), in.DocName, ext, in.Path, created, body.String())
	if failed.Len() > 0 {
		text += "\n\nFailed:" + failed.String()
	}
	return render.SuccessResult(front, text)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
