package listener

import (
	"errors"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
)

// snapshot is the addon's remote access settings as of the last read, with
// its allowed IP list already parsed.
type snapshot struct {
	Settings addoninstall.RemoteSettings
	// AllowedPrefixes is Settings.AllowedIPs parsed with
	// domain.ParseAllowedIPs. It is nil whenever the allowed list could not
	// be read or did not parse (AllowedErr is then set too), so allowedIn
	// admits loopback only until the file is fixed.
	AllowedPrefixes []netip.Prefix
	AllowedErr      error
}

// settingsCache serves the current remote access settings, re-read from
// target's settings file (or held fixed by override, the TUI's Test
// connection) at most every settingsPollInterval and only when the file's
// modification time or size changed. It is safe for concurrent use.
type settingsCache struct {
	target   addoninstall.Target
	override *addoninstall.RemoteSettings
	log      *log.Logger

	mu        sync.Mutex
	checkedAt time.Time
	// modTime and size are the settings file's state as of the last file
	// state this cache has already acted on, whether that reading
	// succeeded or the allowed list turned out unusable: advancing them in
	// both cases means a still-broken, unedited file is read (and so
	// logged) only once, not on every poll; the next read is triggered only
	// once the file's modification time or size actually changes again.
	modTime time.Time
	size    int64
	current snapshot
}

// newSettingsCache returns a cache for target, or one fixed at *override
// when override is not nil. logger receives one line each time the allowed
// list starts or stops failing to parse.
func newSettingsCache(target addoninstall.Target, override *addoninstall.RemoteSettings, logger *log.Logger) *settingsCache {
	c := &settingsCache{target: target, override: override, log: logger}
	c.current.Settings = addoninstall.DefaultRemoteSettings()
	if override != nil {
		c.current.Settings = *override
	}
	c.applyAllowedIPs()
	return c
}

// applyAllowedIPs parses the current Settings.AllowedIPs, setting
// AllowedPrefixes to nil (loopback only) rather than keeping whatever it
// was before when the value does not parse: used for Options.Override,
// which has no earlier "last good" list of its own to fall back to.
func (c *settingsCache) applyAllowedIPs() {
	prefixes, err := domain.ParseAllowedIPs(c.current.Settings.AllowedIPs)
	c.current.AllowedErr = err
	c.current.AllowedPrefixes = nil
	if err == nil {
		c.current.AllowedPrefixes = prefixes
	}
}

// snapshot returns the current settings, re-reading the file when it has
// changed and settingsPollInterval has passed since the last check.
func (c *settingsCache) snapshot() snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.override != nil {
		if c.current.Settings != *c.override {
			c.current.Settings = *c.override
			c.applyAllowedIPs()
		}
		return c.current
	}

	now := time.Now()
	if !c.checkedAt.IsZero() && now.Sub(c.checkedAt) < settingsPollInterval {
		return c.current
	}
	c.checkedAt = now

	path := addoninstall.SettingsPath(c.target)
	var modTime time.Time
	var size int64
	if info, err := os.Stat(path); err == nil {
		modTime, size = info.ModTime(), info.Size()
	}
	if modTime.Equal(c.modTime) && size == c.size {
		return c.current
	}

	settings, err := addoninstall.ReadRemoteSettings(c.target)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			// A transient OS-level read failure (permission, an I/O error):
			// keep serving the previous settings and do not advance
			// modTime/size, so the next poll retries this same file state
			// rather than treating it as a new, broken one.
			return c.current
		}
		// The file itself is not valid JSON, so there is no allowed list to
		// read from it at all: the same fail-safe as a bad allowed_ips
		// value below.
		c.allowListFailed(path, modTime, size, err)
		return c.current
	}

	// ReadRemoteSettings does not validate allowed_ips itself, so a value
	// such as "192.168.1.0/33" reaches here needing its own check.
	prefixes, perr := domain.ParseAllowedIPs(settings.AllowedIPs)
	if perr != nil {
		c.allowListFailed(path, modTime, size, perr)
		return c.current
	}

	if c.current.AllowedErr != nil {
		c.log.Printf("listener: %s is valid again", path)
	}
	c.modTime, c.size = modTime, size
	c.current.Settings = settings
	c.current.AllowedPrefixes = prefixes
	c.current.AllowedErr = nil
	return c.current
}

// allowListFailed records that the settings file at modTime/size could not
// yield a usable allowed list, whether because the file itself is not
// valid JSON or because allowed_ips within it does not parse. Every other
// setting keeps its last good value (the port, remote_enabled, the
// password): only the allow list becomes loopback-only (AllowedPrefixes
// nil) until the file is fixed, rather than resetting everything to
// defaults or silently keeping a possibly stale old list. Advancing
// modTime/size here, even though the read did not otherwise change
// anything applied, is what makes the log line run once per distinct
// broken file content instead of on every poll while it stays broken.
func (c *settingsCache) allowListFailed(path string, modTime time.Time, size int64, err error) {
	c.modTime, c.size = modTime, size
	c.current.AllowedPrefixes = nil
	c.current.AllowedErr = err
	// err may be about allowed_ips specifically, or about the settings file
	// as a whole (invalid JSON, or another value of the wrong type, such as
	// a non-string auth_token): named generically here since either way the
	// effect on the listener is the same, only loopback allowed. Logged
	// through shouldLogSettingsBroken, not unconditionally: this cache
	// instance's own modTime/size (above) only dedupes within one cache's
	// life, but listen.go's retry loop builds a fresh listener.Run, and so a
	// fresh settingsCache, on every attempt while the listener cannot start;
	// without a dedup that survives that, the very first snapshot() of each
	// new instance would log this again every 30 s for the very same,
	// unfixed file (live check N11).
	if shouldLogSettingsBroken(path, modTime, size) {
		c.log.Printf("listener: %s: the settings file cannot be used, only loopback is allowed until it is fixed: %v", path, err)
	}
}

// loggedSettingsBroken remembers, per settings file path, the modTime/size
// this process last logged allowListFailed for, across every settingsCache
// instance (see shouldLogSettingsBroken).
var (
	loggedSettingsBrokenMu sync.Mutex
	loggedSettingsBroken   = map[string]struct {
		modTime time.Time
		size    int64
	}{}
)

// shouldLogSettingsBroken reports whether allowListFailed should log for
// path at modTime/size, and records that it now has: false when this exact
// file state was already logged, by this settingsCache or an earlier one
// for the same path.
func shouldLogSettingsBroken(path string, modTime time.Time, size int64) bool {
	loggedSettingsBrokenMu.Lock()
	defer loggedSettingsBrokenMu.Unlock()
	if last, ok := loggedSettingsBroken[path]; ok && last.modTime.Equal(modTime) && last.size == size {
		return false
	}
	loggedSettingsBroken[path] = struct {
		modTime time.Time
		size    int64
	}{modTime, size}
	return true
}

// allowed reports whether addr may reach the listener, fetching a fresh
// snapshot to check it against.
func (c *settingsCache) allowed(addr netip.Addr) bool {
	return allowedIn(c.snapshot(), addr)
}

// allowedIn reports whether addr may reach the listener under snap:
// loopback (127.0.0.0/8, ::1, an IPv4-mapped address unmapped first, any
// zone dropped) is always allowed; otherwise addr must be inside snap's
// allowed_ips prefixes (none, so nothing but loopback, while they fail to
// parse). Taking snap directly, rather than fetching it itself, lets a
// caller that already has one (handler.ServeHTTP) reuse it instead of
// taking settingsCache's lock a second time in the same request.
func allowedIn(snap snapshot, addr netip.Addr) bool {
	addr = addr.Unmap().WithZone("")
	if addr.IsLoopback() {
		return true
	}
	for _, p := range snap.AllowedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
