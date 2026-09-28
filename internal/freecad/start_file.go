package freecad

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StartFileError is a start_freecad file argument that was refused. Message
// says why and Hint what to do; both are shown to the model as is.
type StartFileError struct {
	Message string
	Hint    string
}

func (e *StartFileError) Error() string { return e.Message }

// ValidateStartFile checks the file argument of start_freecad on the machine
// that runs FreeCAD: an absolute path to an existing .FCStd file. It returns
// the path, or a *StartFileError.
func ValidateStartFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", &StartFileError{
			Message: fmt.Sprintf("file must be an absolute path; got %q.", path),
			Hint:    "Pass an absolute path, or omit file to start FreeCAD without opening a document.",
		}
	}
	if err := checkFCStdExt(path); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", &StartFileError{
			Message: fmt.Sprintf("file does not exist: %s", path),
			Hint:    "Check the path, or omit file to start FreeCAD without opening a document.",
		}
	}
	return path, nil
}

// CheckStartFileSyntax checks a file argument that names a path on another
// computer, by syntax only: an absolute path in POSIX (/home/me/a.FCStd) or
// Windows form (C:\Users\me\a.FCStd, \\server\share\a.FCStd) ending in
// .FCStd. The listener on that computer checks that the file exists.
func CheckStartFileSyntax(path string) error {
	windowsAbs := len(path) >= 3 && isDriveLetter(path[0]) && path[1] == ':' && (path[2] == '\\' || path[2] == '/') ||
		strings.HasPrefix(path, `\\`)
	if !strings.HasPrefix(path, "/") && !windowsAbs {
		return &StartFileError{
			Message: fmt.Sprintf("file must be an absolute path on the computer running FreeCAD; got %q.", path),
			Hint:    "Pass an absolute path on that computer, or omit file to start FreeCAD without opening a document.",
		}
	}
	return checkFCStdExt(path)
}

func checkFCStdExt(path string) error {
	name := path
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if !strings.HasSuffix(strings.ToLower(name), ".fcstd") {
		return &StartFileError{
			Message: fmt.Sprintf("file must be a .FCStd file; got %q.", path),
			Hint:    "Pass the path of a .FCStd document, or omit file. Use import_file after FreeCAD has started for other formats.",
		}
	}
	return nil
}

func isDriveLetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
