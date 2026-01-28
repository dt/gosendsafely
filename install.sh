#!/bin/sh
set -e

REPO="dt/gosendsafely"

# Use INSTALL_DIR if set, otherwise pick first candidate directory that exists and is in PATH
if [ -z "$INSTALL_DIR" ]; then
  for dir in "$HOME/bin" "$HOME/.local/bin" "/usr/local/bin"; do
    case ":$PATH:" in
      *":$dir:"*) [ -d "$dir" ] && INSTALL_DIR="$dir" && break ;;
    esac
  done
fi

# Fallback to current directory if none found in PATH
if [ -z "$INSTALL_DIR" ]; then
  INSTALL_DIR="."
  INSTALL_DIR_FALLBACK=1
fi

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin) OS="darwin" ;;
  linux) OS="linux" ;;
  *)
    echo "Unsupported OS: $OS"
    exit 1
    ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64|amd64) ARCH="amd64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

SUFFIX="${OS}-${ARCH}"

# Check if we have binaries for this platform
case "$SUFFIX" in
  darwin-arm64|linux-amd64) ;;
  *)
    echo "No prebuilt binaries available for $SUFFIX"
    echo "Please build from source: go install github.com/$REPO/cmd/...@latest"
    exit 1
    ;;
esac

echo "Detected platform: $SUFFIX"


# Get latest release tag
LATEST=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
if [ -z "$LATEST" ]; then
  echo "Failed to fetch latest release"
  exit 1
fi

echo "Latest release: $LATEST"
echo "Installing to: $INSTALL_DIR"

# Download binaries
for BINARY in ssget ssunzip; do
  URL="https://github.com/$REPO/releases/download/$LATEST/${BINARY}-${SUFFIX}"
  DEST="$INSTALL_DIR/$BINARY"

  echo "Downloading $BINARY..."
  curl -fsSL -o "$DEST" "$URL"
  chmod +x "$DEST"

  # On macOS, remove quarantine attribute to bypass Gatekeeper
  if [ "$OS" = "darwin" ]; then
    xattr -d com.apple.quarantine "$DEST" 2>/dev/null || true
  fi

  echo "Installed $DEST"
done

# Warn if we used fallback or install dir not in PATH
if [ -n "$INSTALL_DIR_FALLBACK" ]; then
  echo ""
  echo "Note: Could not find a writable install directory on PATH."
  echo "Downloaded to current directory; move ssget and ssunzip to a directory on PATH."
else
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
      echo ""
      echo "Note: $INSTALL_DIR is not in your PATH"
      echo "Add it to your shell profile:"
      echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
      ;;
  esac
fi

echo ""
echo "Done!"
