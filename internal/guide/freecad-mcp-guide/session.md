# Sharing FreeCAD with other agents

Applies when remote access is on. release_session and close_freecad are listed only then.

## How the session works

- One agent at a time holds FreeCAD. Your first call, except start_freecad and get_rpc_status, claims it for you.
- The claim ends when you stay idle for the configured time (30 minutes by default), or when you release it.
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
