# Sharing FreeCAD with other agents

Applies when remote access is on. release_session and close_freecad are listed only then.

## How the session works

- One agent at a time holds FreeCAD. Your first call, except start_freecad and get_rpc_status, claims it for you.
- The claim ends when you stay idle for the configured time (30 minutes by default), or when you release it.
- While one of your execute_code_headless scripts runs, in the foreground or the background, your claim does not count as idle: the server refreshes it every 60 seconds. Normal idle timing resumes when the script ends.
- While an agent works, FreeCAD shows a banner over the 3D view naming the agent, what it is doing and for how long, and asking the person not to edit. It never blocks the mouse or keyboard. A call that may block FreeCAD shows it for its whole run, so even a short one shows it briefly. With remote access on, the agent named is the one that holds FreeCAD.
- get_rpc_status shows who holds FreeCAD and when it frees. It works for every agent.

## When you stop

1. Save your documents with save_document or save_document_as.
2. Call release_session to free FreeCAD and keep it running, or close_freecad to quit it.
3. Do this at every long break too.

release_session frees only your own session. Documents stay open and unsaved changes stay unsaved.

close_freecad is refused while documents have unsaved changes. Save first, or pass discard_changes true to drop them. It is also refused while a task panel or command is open in FreeCAD. start_freecad starts FreeCAD again.

## When a call is refused as in use

- Another agent holds FreeCAD. The error names it and says when it frees.
- Call get_rpc_status. Wait until it shows FreeCAD free, then retry.
- Do not retry in a tight loop. Wait at least the time the error gives, or ask the user.
- The person at the FreeCAD computer can free it with Force release.

## When your session was released

- A refusal that says the person released your session means FreeCAD may have changed.
- Call get_rpc_status, then list_documents, and check the documents before you continue.
