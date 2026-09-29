package winsession

import "testing"

func TestHiddenIsSessionZeroOnly(t *testing.T) {
	if !Hidden(0) {
		t.Error("session 0 has no desktop")
	}
	for _, id := range []uint32{1, 2, 65536} {
		if Hidden(id) {
			t.Errorf("session %d has a desktop", id)
		}
	}
}
