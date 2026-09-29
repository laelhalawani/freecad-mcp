package freecad

import "context"

// ReleaseSession frees the session lock this session holds (release_session).
func (c *Connection) ReleaseSession(ctx context.Context) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "release_session")
}

// KeepSessionAlive counts as activity of the session that holds the lock, so
// it does not expire while a headless job runs; it claims nothing and is never
// refused (keep_alive). job, activity and elapsedSeconds also tell the banner
// over FreeCAD's 3D view what the job is and for how long it has run; ending
// says it is over.
func (c *Connection) KeepSessionAlive(ctx context.Context, job, activity string, elapsedSeconds float64, ending bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "keep_alive", activity, elapsedSeconds, ending, job)
}

// CloseFreeCAD closes every document and quits FreeCAD (close_freecad).
// Unsaved changes are refused unless discardChanges is set.
func (c *Connection) CloseFreeCAD(ctx context.Context, discardChanges bool) (map[string]any, error) {
	return c.callMap(ctx, c.timeout, "close_freecad", discardChanges)
}
