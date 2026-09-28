//go:build !windows && !darwin && !linux

package autostart

import "github.com/sairaph/freecad-mcp/internal/listenerapi"

func register(Entry) error { return ErrUnsupported }

func unregister() error { return nil }

func start() error { return ErrUnsupported }

func stop() error { return nil }

func restart() error { return ErrUnsupported }

// status reports no registration mechanism, but still checks whether a
// listener happens to be running (for example started by hand with
// "freecad-mcp listen"), so the doctor is not wrong about that on an
// unsupported OS.
func status() (State, error) {
	return State{Detail: "starting with the session is not supported on this operating system", Running: listenerapi.Running()}, nil
}
