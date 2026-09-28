package listener

import (
	"log"
	"sync"
	"time"
)

// maxRateLogEntries caps how many distinct keys rateLimitedLog tracks at
// once. Once full, a key seen for the first time logs one generic overflow
// notice instead of being tracked (and so logged) individually, so a wide
// scan from many distinct sources cannot grow this map without bound.
const maxRateLogEntries = 4096

// overflowKey is a sentinel that no real bucket|reason key can collide
// with, used to throttle the overflow notice itself to once a minute.
const overflowKey = "\x00overflow"

// rateLimitedLog logs at most one line per key per minute, so a flood of
// identical rejections from one source (a scanner retrying a closed door,
// or a client stuck retrying a wrong password) cannot fill the log.
type rateLimitedLog struct {
	log *log.Logger

	mu          sync.Mutex
	last        map[string]time.Time
	lastPruneAt time.Time
}

func newRateLimitedLog(l *log.Logger) *rateLimitedLog {
	return &rateLimitedLog{log: l, last: map[string]time.Time{}}
}

// Printf logs format/args under key, unless the same key was already
// logged within the last minute. Pruning of idle keys runs on an interval
// (like backoff.go), never as a full sweep on every call, so it cannot slow
// down a hot path such as allowedListener.Accept.
func (r *rateLimitedLog) Printf(key, format string, args ...any) {
	r.mu.Lock()
	now := time.Now()
	if now.Sub(r.lastPruneAt) > pruneInterval {
		r.pruneLocked(now)
		r.lastPruneAt = now
	}

	last, tracked := r.last[key]
	switch {
	case tracked && now.Sub(last) < time.Minute:
		r.mu.Unlock()
		return
	case !tracked && len(r.last) >= maxRateLogEntries:
		overflowLast, ok := r.last[overflowKey]
		if ok && now.Sub(overflowLast) < time.Minute {
			r.mu.Unlock()
			return
		}
		r.last[overflowKey] = now
		r.mu.Unlock()
		r.log.Printf("listener: further rejections suppressed (too many distinct sources)")
	default:
		r.last[key] = now
		r.mu.Unlock()
		r.log.Printf(format, args...)
	}
}

// pruneLocked removes keys past the throttle window, so a source that
// stops (a flood that ends, an address that leaves) is logged again soon
// after, rather than staying silently suppressed by a now-idle entry.
// Callers must hold r.mu.
func (r *rateLimitedLog) pruneLocked(now time.Time) {
	for k, t := range r.last {
		if now.Sub(t) > time.Minute {
			delete(r.last, k)
		}
	}
}
