package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/laelhalawani/freecad-mcp/internal/domain"
	"github.com/laelhalawani/freecad-mcp/internal/freecad"
	"github.com/laelhalawani/freecad-mcp/internal/xmlrpc"
	"github.com/sairaph/mcp-wizard/render"
)

// connector hands out the FreeCAD connection. It connects on first use and
// keeps the connection only once FreeCAD has answered, so a server started
// before FreeCAD (the usual order, since AI clients launch it) checks the
// addon version on the first call that reaches FreeCAD.
type connector struct {
	settings domain.Settings
	version  string
	dial     func() *freecad.Connection

	mu     sync.Mutex
	conn   *freecad.Connection
	notice string // addon version warning not yet shown in a tool reply
}

func newConnector(settings domain.Settings, version string) *connector {
	c := &connector{settings: settings, version: version}
	c.dial = func() *freecad.Connection {
		return freecad.NewConnection(settings.Host, settings.Port, settings.Token, freecad.DefaultTimeout)
	}
	return c
}

// get returns the connection, connecting and checking the addon version the
// first time FreeCAD answers.
func (c *connector) get(ctx context.Context) (*freecad.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	conn := c.dial()
	// ping never waits for FreeCAD's GUI thread, so a healthy addon answers at
	// once; a short bound keeps a stalled host from holding the lock (and
	// every other tool call) for the full reply timeout.
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	ok, err := conn.Ping(pingCtx)
	cancel()
	if err != nil {
		conn.Close()
		var perr *xmlrpc.ProtocolError
		if errors.As(err, &perr) && perr.StatusCode == 401 {
			return nil, &toolError{render.Error{
				Code: render.CodeAuth,
				Message: "FreeCAD rejected the connection: the addon requires an auth token, " +
					"and none or a different one was given.",
				Hint: "Run `" + domain.BinaryName + " login --token <token>` with the token set with " +
					"'Set Auth Token' in FreeCAD, or set " + domain.EnvToken + " in the AI client's config for this server.",
			}}
		}
		return nil, &toolError{render.Error{
			Code:    render.CodeUnavailable,
			Message: fmt.Sprintf("Failed to connect to FreeCAD at %s (%v). Make sure the FreeCAD addon is running.", conn.URL(), err),
			Hint:    startHint,
		}}
	}
	if !ok {
		conn.Close()
		return nil, &toolError{render.Error{
			Code:    render.CodeUnavailable,
			Message: "Failed to connect to FreeCAD: the addon did not answer ping. Make sure the FreeCAD addon is running.",
			Hint:    startHint,
		}}
	}
	c.notice = conn.CheckAddonVersion(ctx, c.version)
	c.conn = conn
	return conn, nil
}

// takeNotice returns the pending version warning once.
func (c *connector) takeNotice() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.notice
	c.notice = ""
	return n
}

func (c *connector) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

const pingTimeout = 10 * time.Second

const startHint = "Start FreeCAD, select the MCP Addon workbench and click Start RPC Server " +
	"(or turn on its auto-start), then retry. Run `" + domain.BinaryName + " doctor` to check the setup."
