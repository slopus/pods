#!/bin/sh
# Happy Pods CLI installer.
#
#   curl -fsSL https://podbay.dev/install.sh | sh
#   curl -fsSL https://github.com/slopus/pods/releases/latest/download/install.sh | sh
#
# Downloads the right `pods` binary for your OS/CPU from the latest GitHub
# release and installs it. Override with env vars:
#   PODS_VERSION   release tag to install (default: latest)
#   PODS_INSTALL_DIR  install directory (default: /usr/local/bin, else ~/.local/bin)
set -eu

REPO="slopus/pods"
VERSION="${PODS_VERSION:-latest}"

info() { printf '\033[1;36m==>\033[0m %s\n' "$1"; }
err() { printf '\033[1;31merror:\033[0m %s\n' "$1" >&2; exit 1; }

# --- detect OS -------------------------------------------------------------
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS \"$os\" (Linux and macOS are supported; on Windows download pods-windows-amd64.exe from the releases page)" ;;
esac

# --- detect CPU ------------------------------------------------------------
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported CPU architecture \"$arch\"" ;;
esac

asset="pods-${os}-${arch}"

# --- resolve download URL --------------------------------------------------
if [ "$VERSION" = latest ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

# --- pick an install dir ---------------------------------------------------
if [ -n "${PODS_INSTALL_DIR:-}" ]; then
  bindir="$PODS_INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  bindir=/usr/local/bin
elif [ "$(id -u)" = 0 ]; then
  bindir=/usr/local/bin
else
  bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir" 2>/dev/null || err "cannot create install dir $bindir (set PODS_INSTALL_DIR)"

# --- fetch -----------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }
have curl || have wget || err "need curl or wget installed"

tmp=$(mktemp "${TMPDIR:-/tmp}/pods.XXXXXX") || err "cannot create temp file"
trap 'rm -f "$tmp"' EXIT INT TERM

info "downloading $asset ($VERSION)"
if have curl; then
  curl -fsSL "$url" -o "$tmp" || err "download failed: $url"
else
  wget -qO "$tmp" "$url" || err "download failed: $url"
fi

# Guard against an HTML error page sneaking in as the "binary".
if head -c 4 "$tmp" | LC_ALL=C grep -q '<'; then
  err "download did not return a binary (got an HTML page from $url)"
fi

chmod +x "$tmp"

# --- install (sudo only if needed) -----------------------------------------
target="$bindir/pods"
if [ -w "$bindir" ]; then
  mv "$tmp" "$target"
elif have sudo; then
  info "installing to $target (needs sudo)"
  sudo mv "$tmp" "$target"
else
  err "no write permission for $bindir and sudo is unavailable (set PODS_INSTALL_DIR to a writable dir)"
fi
trap - EXIT INT TERM

info "installed $("$target" version 2>/dev/null || echo pods) -> $target"

case ":$PATH:" in
  *":$bindir:"*) ;;
  *) printf '\033[1;33mnote:\033[0m %s is not on your PATH. Add it:\n  export PATH="%s:$PATH"\n' "$bindir" "$bindir" ;;
esac

cat <<'EOF'

Next:
  pods login --endpoint https://podbay.dev   # sign in with GitHub
  pods init hello && pods deploy hello        # live at https://hello.podbay.dev
EOF
