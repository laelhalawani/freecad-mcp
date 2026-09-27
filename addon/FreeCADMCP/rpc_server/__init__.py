# InitGui.py puts the addon folder on sys.path, so this folder is the
# top-level package ``rpc_server``. Its modules import one another by absolute
# name (``from rpc_server.settings import ...``); relative imports such as
# ``from . import rpc_server`` in commands.py resolve to the same modules.
# FreeCAD runs InitGui.py itself outside any package, so it can only use the
# absolute form (``from rpc_server import rpc_server``).
