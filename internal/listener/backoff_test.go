package listener

import (
	"net/netip"
	"testing"
)

// TestBackoffBlocksAfterFiveFailures covers live check L5: five wrong
// passwords from one address are each still answered normally (401, not
// blocked); only the sixth, and every one after it within the block
// window, is blocked (429), matching what the live run observed with seven
// wrong passwords 15 ms apart.
func TestBackoffBlocksAfterFiveFailures(t *testing.T) {
	b := newBackoff()
	addr := netip.MustParseAddr("192.168.1.50")

	for i := 1; i <= 5; i++ {
		if blocked := b.checkAndRecord(addr, false, true); blocked {
			t.Fatalf("failure %d: blocked, want not blocked (401)", i)
		}
	}
	for i := 6; i <= 7; i++ {
		if blocked := b.checkAndRecord(addr, false, true); !blocked {
			t.Fatalf("failure %d: not blocked, want blocked (429)", i)
		}
	}
	// The right password, tried while blocked, is still refused: the block
	// applies to the address, not to whichever password it last sent.
	if blocked := b.checkAndRecord(addr, true, true); !blocked {
		t.Fatal("right password while blocked: not blocked, want blocked")
	}
}

// TestBackoffNeverCountsARequestWithNoPassword covers the "only a request
// that carried an Authorization header at all counts" rule: countFailure
// false (no Authorization header) never advances a bucket toward the
// limit, however many times it happens.
func TestBackoffNeverCountsARequestWithNoPassword(t *testing.T) {
	b := newBackoff()
	addr := netip.MustParseAddr("10.0.0.9")
	for i := 0; i < 20; i++ {
		if blocked := b.checkAndRecord(addr, false, false); blocked {
			t.Fatalf("request %d with no Authorization header: blocked", i)
		}
	}
}

// TestBackoffBucketsAreIndependent covers that the limit is per address
// bucket: a blocked address does not block a different one.
func TestBackoffBucketsAreIndependent(t *testing.T) {
	b := newBackoff()
	blocked := netip.MustParseAddr("192.168.1.50")
	other := netip.MustParseAddr("192.168.1.51")
	for i := 0; i < 5; i++ {
		b.checkAndRecord(blocked, false, true)
	}
	if !b.checkAndRecord(blocked, false, true) {
		t.Fatal("the address with 6 failures should be blocked")
	}
	if b.checkAndRecord(other, false, true) {
		t.Fatal("a different address should not be blocked by another one's failures")
	}
}
