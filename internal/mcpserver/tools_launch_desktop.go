package mcpserver

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/freecad-mcp/internal/addoninstall"
	"github.com/sairaph/freecad-mcp/internal/domain"
	"github.com/sairaph/freecad-mcp/internal/listenerapi"
	"github.com/sairaph/freecad-mcp/internal/remote"
	"github.com/sairaph/freecad-mcp/internal/winsession"
	"github.com/sairaph/mcp-wizard/render"
)

// inHiddenSession reports whether this server's own process runs in a Windows
// session with no desktop (started from an SSH login or a service): a FreeCAD
// it started itself would be invisible to the user. A session that cannot be
// read counts as not hidden, never a guess.
func (s *Server) inHiddenSession() bool {
	if s.sessionOf == nil {
		return false
	}
	id, err := s.sessionOf(os.Getpid())
	return err == nil && winsession.Hidden(id)
}

// localListenerEndpoint is the freecad-mcp listener on this computer: on
// 127.0.0.1, at the listener_port of the addon settings file in FreeCAD's user
// data directory, with the password stored there. The listener always admits
// loopback, whatever allowed_ips lists. token (the stored password this server
// was configured with) stands in when the settings cannot be found.
func localListenerEndpoint(ctx context.Context, token string) remote.Endpoint {
	ep := remote.Endpoint{Host: "127.0.0.1", Port: addoninstall.DefaultListenerPort, Token: token}
	if targets := addoninstall.Locate(ctx, nil); len(targets) > 0 {
		if st, err := addoninstall.ReadRemoteSettings(targets[0]); err == nil {
			ep.Port, ep.Token = st.ListenerPort, st.AuthToken
		}
	}
	return ep
}

// startThroughDesktopListener is start_freecad's local path when this server
// has no desktop: it asks the listener on this computer, which runs in the
// user's own session, to start FreeCAD, so FreeCAD shows on the desktop. It
// never launches FreeCAD itself.
func (s *Server) startThroughDesktopListener(ctx context.Context, file string, rawFile *string) (*mcp.CallToolResult, any, error) {
	ep := s.desktopListener(ctx, s.config.FreeCAD.Token)
	res, err := remote.Start(ctx, ep, file)
	if err != nil {
		if lerr := listenerCallError(ep.Host, ep.Port, err); lerr != nil {
			return s.withNotice(render.ErrorResult(*lerr)), nil, nil
		}
		// Nothing answered on the listener's port, or something that is not a
		// listener did.
		return s.withNotice(render.ErrorResult(noDesktopError())), nil, nil
	}
	if res.State == listenerapi.StateAlreadyRunning {
		if inUse := s.sessionHeldByAnother(ctx); inUse != nil {
			return s.withNotice(failure(ctx, "start FreeCAD", inUse, "")), nil, nil
		}
		return s.withNotice(alreadyRunningResult(s.config.FreeCAD.Port, "", rawFile)), nil, nil
	}
	// The launch is the listener's; adopting it lets get_rpc_status follow it
	// (starting, then reachable, or its log if FreeCAD never answers).
	ls := res.FreeCAD.LaunchState(time.Now())
	s.launcher.Adopt(ls)
	s.fc.reset()
	return s.withNotice(launchResult(ls, func() string { return res.FreeCAD.LogTail }, file, "")), nil, nil
}

// noDesktopError is start_freecad's refusal when this server has no desktop
// and no listener on this computer starts FreeCAD for it.
func noDesktopError() render.Error {
	return render.Error{
		Code: render.CodeUnavailable,
		Message: "This agent runs outside the user's desktop session (for example over SSH), so a FreeCAD it starts " +
			"would be invisible.",
		Hint: fmt.Sprintf("Ask the user to start FreeCAD on their desktop, or to turn on sharing (`%s share --on`), "+
			"which runs the listener on their desktop; execute_code_headless works without a desktop.", domain.BinaryName),
	}
}
