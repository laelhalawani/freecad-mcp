package winsession

import "golang.org/x/sys/windows"

// Of returns the session of process pid (ProcessIdToSessionId).
func Of(pid int) (uint32, error) {
	var id uint32
	if err := windows.ProcessIdToSessionId(uint32(pid), &id); err != nil {
		return 0, err
	}
	return id, nil
}
