//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
)

// removeFromUserPath removes the lines install.sh appended to the shell
// profiles; that is the only place it put the install directory on PATH.
func removeFromUserPath(dir string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	removedAny := false
	var errs []error
	for _, name := range []string{".zshrc", ".bashrc", ".profile", ".bash_profile"} {
		path := filepath.Join(home, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		updated, removed := removeRCBlock(string(data), dir)
		if !removed {
			continue
		}
		if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
			errs = append(errs, err)
			continue
		}
		removedAny = true
	}
	return removedAny, errors.Join(errs...)
}

// removeInstallRoot deletes root; a running program may delete its own file
// on Unix.
func removeInstallRoot(root string) error {
	return os.RemoveAll(root)
}
