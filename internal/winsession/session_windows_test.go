package winsession

import (
	"os"
	"testing"
)

// TestOfReadsThisProcessSession runs the real ProcessIdToSessionId call: a
// process that exists has a session, and a pid that does not is an error, not
// session 0.
func TestOfReadsThisProcessSession(t *testing.T) {
	if _, err := Of(os.Getpid()); err != nil {
		t.Fatalf("Of(own pid): %v", err)
	}
	if id, err := Of(0x7ffffff0); err == nil {
		t.Fatalf("Of(no such pid) = %d, nil; want an error", id)
	}
}
