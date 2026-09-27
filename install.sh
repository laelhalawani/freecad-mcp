#!/bin/sh
# Installs freecad-mcp from GitHub releases and runs its setup wizard.
# Cancelling the wizard undoes everything this script changed.
#
#   curl -fsSL https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.sh | sh
#
# Environment:
#   VERSION=v0.2.1           install a specific release instead of the latest
#   CONFIGURE_ARGS="--yes"   extra flags for the setup wizard
OWNER="laelhalawani"
REPO="freecad-mcp"
BIN="freecad-mcp"
VERSION="${VERSION:-latest}"
CONFIGURE_ARGS="${CONFIGURE_ARGS:-}"
# `freecad-mcp configure` exits with 3 when the wizard is left before it
# changed anything.
EXIT_CANCELLED=3
MARKER="# added by ${BIN} installer"

# --- shell profile edits, undone exactly on cancel --------------------------
# RC_STATE is a private directory holding, for each profile this run
# changed, a copy of the original and the text that was appended.

rc_append() { # rc_append <profile> <line>
  rc="$1"; line="$2"
  key=$(printf '%s' "$rc" | cksum | awk '{print $1}')
  cp -p "$rc" "$RC_STATE/$key.orig"
  printf '%s\n' "$rc" > "$RC_STATE/$key.path"
  : > "$RC_STATE/$key.added"
  # End an unterminated last line first, so the block starts on its own line.
  if [ -s "$rc" ] && [ -n "$(tail -c 1 "$rc")" ]; then
    printf '\n' >> "$RC_STATE/$key.added"
  fi
  printf '\n%s\n%s\n' "$MARKER" "$line" >> "$RC_STATE/$key.added"
  cat "$RC_STATE/$key.added" >> "$rc"
}

rc_undo() { # restores every profile rc_append changed in this run
  for pathfile in "$RC_STATE"/*.path; do
    [ -f "$pathfile" ] || continue
    key=$(basename "$pathfile" .path)
    rc=$(cat "$pathfile")
    cat "$RC_STATE/$key.orig" "$RC_STATE/$key.added" > "$RC_STATE/$key.expected"
    if cmp -s "$rc" "$RC_STATE/$key.expected"; then
      # Unchanged since: put the original back byte for byte.
      cat "$RC_STATE/$key.orig" > "$rc"
    else
      # Edited meanwhile: remove only the two lines this run added.
      awk -v marker="$MARKER" -v line="$(tail -n 1 "$RC_STATE/$key.added")" '
        $0 == marker { held = 1; next }
        held == 1 && $0 == line { held = 0; next }
        held == 1 { print marker; held = 0 }
        { print }' "$rc" > "$RC_STATE/$key.new" && cat "$RC_STATE/$key.new" > "$rc"
    fi
  done
}

# Tests source this file with FREECAD_MCP_INSTALL_LIB=1 to use the functions.
if [ "${FREECAD_MCP_INSTALL_LIB:-}" = "1" ]; then
  return 0 2>/dev/null || exit 0
fi

set -e

# --- detect OS / arch ------------------------------------------------------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux*)  os=linux ;;
  Darwin*) os=darwin ;;
  *) printf '\n  Unsupported OS: %s\n' "$OS" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) printf '\n  Unsupported architecture: %s\n' "$ARCH" >&2; exit 1 ;;
esac

ASSET="${BIN}-${os}-${arch}"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${OWNER}/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${OWNER}/${REPO}/releases/download/${VERSION}"
fi
URL="${BASE}/${ASSET}"
INSTALL_ROOT="$HOME/.${REPO}"
INSTALL_DIR="$INSTALL_ROOT/bin"
TARGET="$INSTALL_DIR/$BIN"

# Record what exists now, so a cancelled setup can put it back.
created_root=0; [ -d "$INSTALL_ROOT" ] || created_root=1
created_dir=0; [ -d "$INSTALL_DIR" ] || created_dir=1
mkdir -p "$INSTALL_DIR"
RC_STATE=$(mktemp -d)

undo_directories() {
  if [ "$created_dir" -eq 1 ]; then rmdir "$INSTALL_DIR" 2>/dev/null || true; fi
  if [ "$created_root" -eq 1 ]; then rmdir "$INSTALL_ROOT" 2>/dev/null || true; fi
}

TEMP="${TARGET}.new"
cleanup() { rm -f "$TEMP" "$TEMP.err"; rm -rf "$RC_STATE"; }
trap cleanup EXIT HUP INT TERM

printf '\n  %s installer\n\n  Downloading %s (%s)...\n' "$BIN" "$ASSET" "$VERSION"

download_failed() {
  printf '\n  Download failed. Please check your connection and try again.\n' >&2
  printf '  URL: %s\n' "$URL" >&2
  if [ -n "$1" ]; then
    printf '  Reason: %s\n' "$1" >&2
  fi
  rm -f "$TEMP" "$TEMP.err"
  undo_directories
  exit 1
}

if command -v curl >/dev/null 2>&1; then
  curl -fSL "$URL" -o "$TEMP" 2>/dev/null || download_failed
elif command -v wget >/dev/null 2>&1; then
  if ! wget -q --show-progress -O "$TEMP" "$URL" 2>"$TEMP.err"; then
    if ! wget -q -O "$TEMP" "$URL" 2>"$TEMP.err"; then
      download_failed "$(cat "$TEMP.err" 2>/dev/null | tr '\n' ' ')"
    fi
  fi
else
  download_failed "neither curl nor wget is available"
fi
rm -f "$TEMP.err"

if command -v sha256sum >/dev/null 2>&1; then
  SHA256_CMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA256_CMD="shasum -a 256"
else
  SHA256_CMD=""
fi

if [ -n "$SHA256_CMD" ]; then
  CHECKSUM_URL="${BASE}/SHA256SUMS.txt"
  EXPECTED=""
  if command -v curl >/dev/null 2>&1; then
    EXPECTED=$(curl -fsSL "$CHECKSUM_URL" 2>/dev/null | grep " $ASSET\$" | awk '{print $1}')
  elif command -v wget >/dev/null 2>&1; then
    EXPECTED=$(wget -q -O - "$CHECKSUM_URL" 2>/dev/null | grep " $ASSET\$" | awk '{print $1}')
  fi
  if [ -n "$EXPECTED" ]; then
    ACTUAL=$($SHA256_CMD "$TEMP" | awk '{print $1}')
    if [ "$EXPECTED" != "$ACTUAL" ]; then
      printf '\n  SHA256 mismatch; nothing was installed.\n' >&2
      rm -f "$TEMP"
      undo_directories
      exit 1
    fi
  else
    printf '  Warning: could not fetch SHA256SUMS.txt; the download was not verified.\n' >&2
  fi
else
  printf '  Warning: no sha256 tool found; the download was not verified.\n' >&2
fi

if [ ! -s "$TEMP" ]; then
  printf '  Download did not complete; nothing was installed.\n' >&2
  undo_directories
  exit 1
fi
chmod +x "$TEMP"

# Keep a previous version until setup finishes. Its name must not match
# "<bin>.old-*": the program deletes those files when it starts.
BACKUP=""
if [ -f "$TARGET" ]; then
  BACKUP="${TARGET}.bak"
  mv -f "$TARGET" "$BACKUP"
fi
restore_backup() {
  rm -f "$TARGET"
  if [ -n "$BACKUP" ] && [ -f "$BACKUP" ]; then mv -f "$BACKUP" "$TARGET"; fi
}
if ! mv -f "$TEMP" "$TARGET"; then
  printf '\n  Failed to install binary to %s\n' "$TARGET" >&2
  restore_backup
  undo_directories
  exit 1
fi

# Put the install directory on PATH through the shell profiles.
case ":$PATH:" in
  *":$INSTALL_DIR:"*) on_path=1 ;;
  *) on_path=0 ;;
esac
if [ "$on_path" -eq 0 ]; then
  for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile" "$HOME/.bash_profile"; do
    [ -f "$rc" ] || continue
    if ! grep -qF "$INSTALL_DIR" "$rc" 2>/dev/null; then
      rc_append "$rc" "export PATH=\"$INSTALL_DIR:\$PATH\""
    fi
    on_path=2
  done
fi

PATH="$INSTALL_DIR:$PATH"
export PATH

# Run the setup wizard, in a terminal only: without one it cannot ask, and
# `configure` would register every detected client unasked.
if ( : </dev/tty ) 2>/dev/null; then
  set +e
  # shellcheck disable=SC2086
  "$TARGET" configure $CONFIGURE_ARGS </dev/tty
  code=$?
  set -e
else
  printf '\n  Installed %s to %s.\n  Not running in a terminal. Finish setup in one with:\n    %s configure\n' "$BIN" "$TARGET" "$BIN"
  code=0
fi

if [ "$code" -eq "$EXIT_CANCELLED" ]; then
  # Put everything back as it was.
  restore_backup
  rc_undo
  undo_directories
  # The wizard already said "Setup cancelled"; say what was restored.
  if [ -n "$BACKUP" ]; then
    printf '  The previously installed %s was kept.\n' "$BIN"
  else
    printf '  %s was not installed.\n' "$BIN"
  fi
  exit 0
fi

rm -f "$BACKUP"
if [ "$code" -ne 0 ]; then
  printf '  Setup did not finish (exit code %s). %s is installed at %s.\n' "$code" "$BIN" "$TARGET"
  printf '  Run `%s configure` to finish, or `%s uninstall --all` to remove it.\n' "$BIN" "$BIN"
fi

if [ "$on_path" -eq 0 ]; then
  printf '\n  Add this to your shell profile:\n    export PATH="%s:$PATH"\n' "$INSTALL_DIR"
elif [ "$on_path" -eq 2 ]; then
  printf '\n  Open a new terminal so `%s` is on your PATH.\n' "$BIN"
fi
