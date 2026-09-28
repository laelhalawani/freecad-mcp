package listener

import (
	"net/netip"
	"sync"
	"time"
)

// Backoff parameters: 5 failed passwords from one bucket within 60 s block
// every further request from it for 60 s.
const (
	backoffWindow = 60 * time.Second
	backoffBlock  = 60 * time.Second
	backoffLimit  = 5
	// maxBackoffEntries caps how many buckets are tracked at once. Once
	// full, a bucket seen for the first time is blocked outright (fail
	// closed) rather than left untracked, which would let an unbounded
	// number of first-time addresses bypass the backoff entirely.
	maxBackoffEntries = 4096
	// pruneInterval bounds how often the bucket map is swept for stale
	// entries: checkAndRecord runs on every guarded request, so pruning on
	// every call would cost an O(n) scan per request under heavy traffic.
	pruneInterval = 30 * time.Second
)

type ipBackoff struct {
	failures     []time.Time
	blockedUntil time.Time
}

// backoff tracks failed-password attempts per address bucket. It is safe for
// concurrent use.
type backoff struct {
	mu          sync.Mutex
	buckets     map[string]*ipBackoff
	lastPruneAt time.Time
}

func newBackoff() *backoff { return &backoff{buckets: map[string]*ipBackoff{}} }

// backoffKey buckets an IPv6 address by its /64 (a common per-customer
// allocation), so rotating through many addresses in the same subnet cannot
// bypass the limit; an IPv4 address is its own bucket.
func backoffKey(addr netip.Addr) string {
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// checkAndRecord reports whether addr's bucket is blocked. When it is not
// and ok is false, it counts one failure, but only when countFailure is
// true (so a request with no Authorization header at all is never counted),
// and returns the up to date blocked state. Both the check and the record
// happen under one lock, so concurrent requests from the same bucket cannot
// slip past the limit between one deciding it is not blocked and it
// recording its own failure.
func (b *backoff) checkAndRecord(addr netip.Addr, ok, countFailure bool) bool {
	key := backoffKey(addr)

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if now.Sub(b.lastPruneAt) > pruneInterval {
		b.pruneLocked(now)
		b.lastPruneAt = now
	}

	st := b.buckets[key]
	if st != nil && now.Before(st.blockedUntil) {
		return true
	}
	if ok || !countFailure {
		return false
	}

	if st == nil {
		if len(b.buckets) >= maxBackoffEntries {
			b.buckets[key] = &ipBackoff{blockedUntil: now.Add(backoffBlock)}
			return true
		}
		st = &ipBackoff{}
		b.buckets[key] = st
	}

	cutoff := now.Add(-backoffWindow)
	kept := st.failures[:0]
	for _, t := range st.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.failures = append(kept, now)
	// > backoffLimit, not >=: the 5th failed password within the window is
	// still answered 401 like the first 4 (contract: "after 5 failed
	// passwords... every request gets 429"), so blocking starts only once a
	// 6th failure follows the 5 already counted, not on the 5th itself
	// (live check L5, an off-by-one).
	if len(st.failures) > backoffLimit {
		st.blockedUntil = now.Add(backoffBlock)
		st.failures = nil
		return true
	}
	return false
}

// pruneLocked removes buckets with no failures inside the window and no
// active block, so the map does not grow without bound. Callers must hold
// b.mu.
func (b *backoff) pruneLocked(now time.Time) {
	cutoff := now.Add(-backoffWindow)
	for key, st := range b.buckets {
		stale := now.After(st.blockedUntil) &&
			(len(st.failures) == 0 || st.failures[len(st.failures)-1].Before(cutoff))
		if stale {
			delete(b.buckets, key)
		}
	}
}
