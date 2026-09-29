#!/usr/bin/env bash
# dootd installer. The only step that needs SSH, together with `dootd init`.
#
#   curl -fsSL https://github.com/sumitwaani2/dootd/releases/latest/download/install.sh | sudo bash
#   sudo dootd init
#
# What it does (docs/architecture.md §8):
#   1. checks the host (Ubuntu 24.04+, systemd, cgroup v2, x86_64/aarch64)
#   2. downloads dootd-linux-<arch>, verifies it against checksums.txt and
#      installs it to /usr/local/bin/dootd (nothing is changed on a mismatch)
#   3. installs `make` (the only system package apps may use to build)
#   4. `dootd setup-host`: /etc/dootd, /var/lib/dootd, the master key and
#      the systemd unit (enabled; `dootd init` starts it)
#   5. offers a 2 GB swapfile when there is no swap (Zig builds are hungry)
#   6. offers to enable ufw allowing only the SSH port and 443
#
# Answers without a terminal (or to skip the questions):
#   DOOTD_VERSION=v1.0.0   install that release instead of the latest
#   DOOTD_SWAP=yes|no      create the swapfile (default: ask; no terminal = no)
#   DOOTD_UFW=yes|no       enable the firewall (default: ask; no terminal = no)
#   DOOTD_BASE_URL=URL     download from URL/<file> instead of GitHub (testing)
set -euo pipefail

REPO="sumitwaani2/dootd"
INSTALL_PATH="/usr/local/bin/dootd"
MIN_UBUNTU="24.04"
SWAPFILE="/swapfile"

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# version_ge A B: true if A >= B (dotted numeric versions)
version_ge() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]; }

# ask <question> <env value> <default without terminal>: y/n answer.
# `curl | bash` uses stdin for the script, so questions go to /dev/tty.
ask() {
  local q="$1" preset="${2:-}" def="$3" ans=""
  case "$preset" in
    y|yes|Y|YES|1|true) return 0 ;;
    n|no|N|NO|0|false) return 1 ;;
  esac
  if ! (exec </dev/tty) 2>/dev/null; then
    [ "$def" = yes ]; return
  fi
  printf '%s [y/N] ' "$q" >/dev/tty
  read -r ans </dev/tty || true
  case "$ans" in y|Y|yes|YES) return 0 ;; esac
  return 1
}

check_host() {
  [ "$(id -u)" -eq 0 ] || fail "run as root (pipe to 'sudo bash')"
  [ "$(uname -s)" = "Linux" ] || fail "dootd only supports Linux"

  [ -r /etc/os-release ] || fail "cannot read /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = "ubuntu" ] || fail "unsupported OS '${ID:-unknown}': dootd requires Ubuntu ${MIN_UBUNTU} or newer"
  version_ge "${VERSION_ID:-0}" "$MIN_UBUNTU" || fail "Ubuntu ${VERSION_ID:-?} is too old: dootd requires ${MIN_UBUNTU} or newer"

  [ -d /run/systemd/system ] || fail "systemd is not running (required)"
  [ -f /sys/fs/cgroup/cgroup.controllers ] || fail "cgroup v2 (unified hierarchy) is required"

  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "unsupported CPU architecture '$(uname -m)' (supported: x86_64, aarch64)" ;;
  esac

  command -v curl >/dev/null      || fail "curl is required"
  command -v sha256sum >/dev/null || fail "sha256sum is required"
}

download() {
  if [ -n "${DOOTD_BASE_URL:-}" ]; then
    BASE="${DOOTD_BASE_URL%/}"
  elif [ -n "${DOOTD_VERSION:-}" ]; then
    BASE="https://github.com/${REPO}/releases/download/${DOOTD_VERSION}"
  else
    BASE="https://github.com/${REPO}/releases/latest/download"
  fi

  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT

  local bin="dootd-linux-${ARCH}"
  say "Downloading ${bin} (${DOOTD_VERSION:-latest})"
  curl -fsSL --retry 3 -o "${TMP}/${bin}" "${BASE}/${bin}" || fail "download failed: ${BASE}/${bin}"
  curl -fsSL --retry 3 -o "${TMP}/checksums.txt" "${BASE}/checksums.txt" || fail "download failed: ${BASE}/checksums.txt"

  say "Verifying SHA-256"
  local expected actual
  expected="$(awk -v f="$bin" '$2 == f || $2 == "*"f {print $1}' "${TMP}/checksums.txt")"
  [ -n "$expected" ] || fail "no checksum for ${bin} in checksums.txt"
  actual="$(sha256sum "${TMP}/${bin}" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then
    rm -f "${TMP}/${bin}"
    fail "checksum mismatch for ${bin} (expected ${expected}, got ${actual}); nothing was installed"
  fi

  install -m 0755 -o root -g root "${TMP}/${bin}" "${INSTALL_PATH}.new"
  "${INSTALL_PATH}.new" version >/dev/null || { rm -f "${INSTALL_PATH}.new"; fail "the downloaded binary does not run on this machine"; }
  mv -f "${INSTALL_PATH}.new" "$INSTALL_PATH"
  say "Installed: $("$INSTALL_PATH" version)"
}

install_make() {
  if command -v make >/dev/null; then
    return
  fi
  say "Installing make (used by C app builds)"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq make >/dev/null
}

setup_host() {
  say "Setting up directories, master key and the systemd unit"
  "$INSTALL_PATH" setup-host | sed 's/^/    /'
}

maybe_swap() {
  if [ -n "$(swapon --noheadings --show 2>/dev/null)" ]; then
    note "swap: already configured ($(swapon --noheadings --show=SIZE | paste -sd+ -))"
    return
  fi
  if ! ask "No swap found. Create a 2 GB swapfile at ${SWAPFILE} (recommended: Zig builds can use a lot of memory)?" "${DOOTD_SWAP:-}" no; then
    note "swap: skipped"
    return
  fi
  [ ! -e "$SWAPFILE" ] || fail "${SWAPFILE} already exists but is not in use; remove it or enable it yourself"
  say "Creating ${SWAPFILE} (2 GB)"
  fallocate -l 2G "$SWAPFILE" || dd if=/dev/zero of="$SWAPFILE" bs=1M count=2048 status=none
  chmod 0600 "$SWAPFILE"
  mkswap -q "$SWAPFILE" >/dev/null
  swapon "$SWAPFILE"
  grep -qE "^${SWAPFILE}\s" /etc/fstab || echo "${SWAPFILE} none swap sw 0 0" >> /etc/fstab
  note "swap: ${SWAPFILE} enabled and added to /etc/fstab"
}

ssh_port() {
  local p=""
  if command -v sshd >/dev/null; then
    p="$(sshd -T 2>/dev/null | awk '$1 == "port" {print $2; exit}')"
  fi
  echo "${p:-22}"
}

maybe_ufw() {
  if ! command -v ufw >/dev/null; then
    note "firewall: ufw is not installed; make sure only SSH and 443 are reachable"
    return
  fi
  local port; port="$(ssh_port)"
  if ! ask "Enable ufw, allowing only SSH (port ${port}) and 443?" "${DOOTD_UFW:-}" no; then
    note "firewall: skipped (dootd itself only accepts Cloudflare on 443)"
    return
  fi
  ufw allow "${port}/tcp" >/dev/null
  ufw allow 443/tcp >/dev/null
  ufw --force enable >/dev/null
  note "firewall: ufw enabled, allowing ${port}/tcp and 443/tcp"
}

main() {
  check_host
  download
  install_make
  setup_host
  say "Host options"
  maybe_swap
  maybe_ufw
  printf '\n\033[1;32mdootd is installed.\033[0m Next, run:\n\n    sudo dootd init\n\n'
  printf 'It asks for the admin email and password, the dashboard domain and a Cloudflare API token.\n'
  printf 'Rebuilding a server from a recovery kit instead:  sudo dootd init --restore <kit file>\n'
}

main "$@"
