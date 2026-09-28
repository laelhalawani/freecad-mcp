//go:build !windows

package main

// hideConsole has nothing to do outside Windows: the autostart mechanisms on
// macOS and Linux never attach a console to the listener in the first place.
func hideConsole() {}
