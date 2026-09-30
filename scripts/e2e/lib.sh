# shellcheck shell=bash
# Shared helpers for the end-to-end scripts. dootd is driven only the way a
# user drives it: install.sh (served from a local release directory), the
# one-time password it prints, and the dashboard. Fakes:
#   - e2etool cfmock: the Cloudflare API. Its /ips lists 198.18.0.0/15, and
#     198.18.0.10 is added to lo, so requests to that address count as
#     Cloudflare; requests to 127.0.0.1 reach the setup address.
#   - e2etool ghmock: the GitHub API, serving releases from $GHD. Releases
#     of the sample apps are made by their own release workflows
#     (scripts/release-workflow.py), so the path from workflow to deploy is
#     tested end to end.
#   - DOOTD_TEST_* variables in a systemd drop-in point dootd at the fakes
#     (docs/architecture.md §18).
#
#   . "$(dirname "$0")/lib.sh"

set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e"
BIN="/opt/dootd-e2e-bin"   # survives resets (rclone download)
MOCK="$E2E/mock"
OUT="$E2E/out"
REL="$E2E/release"
CF_IP="198.18.0.10"
PUB_IP="203.0.113.10"
D="dootd.example.test"
ADMIN="admin@example.test"
PW="e2e admin password"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
ARCH="$(dpkg --print-architecture)"
DROPIN="/etc/systemd/system/dootd.service.d/test.conf"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }
between() { python3 -c "import sys; v,lo,hi=map(float,sys.argv[1:]); sys.exit(0 if lo<=v<=hi else 1)" "$1" "$2" "$3"; }

# ------------------------------------------------------------ requests

# TLS options for a request "from Cloudflare": any host goes to CF_IP, the
# fake Origin CA is trusted and the AOP client certificate is presented.
CF=(--connect-to "::$CF_IP:443" --cacert "$MOCK/origin-ca.pem" --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key")
cfcurl() { curl -sS -m 60 "${CF[@]}" "$@"; }
# site <host> <path> [args]: a visitor request to an app domain.
site()      { cfcurl -H "CF-Connecting-IP: 203.0.113.7" "${@:3}" "https://$1$2"; }
site_code() { site "$1" "$2" -o /dev/null -w '%{http_code}' "${@:3}" 2>/dev/null; }
site_is()   { [ "$(site_code "$2" "$3" "${@:4}")" = "$1" ]; }
site_has()  { local b; b="$(site "$1" "$2" 2>/dev/null)" || return 1; grep -qF -- "$3" <<<"$b"; }

# The dashboard: use_domain (through Cloudflare) or use_setup (the setup
# address, self-signed). Both keep cookies in $JAR.
JAR="$OUT/jar.txt"
use_domain() { DASH="https://$D"; DOPTS=("${CF[@]}"); JAR="$OUT/jar.txt"; }
use_setup()  { DASH="https://127.0.0.1"; DOPTS=(-k); JAR="$OUT/setup-jar.txt"; }
use_domain
dc()   { curl -sS -m 120 "${DOPTS[@]}" -b "$JAR" -c "$JAR" "$@"; }
get()  { dc "$DASH$1"; }
code() { dc -o /dev/null -w '%{http_code}' "$DASH$1"; }
# post <path> [curl args]: form POST with Origin + CSRF; prints "code location".
post() {
  local p="$1"; shift
  dc -o "$OUT/post.html" -w '%{http_code} %{redirect_url}' -H "Origin: $DASH" --data-urlencode "csrf=${CSRF:-}" "$@" "$DASH$p"
}
post_is() { local want="$1"; shift; local got; got="$(post "$@")"; [[ "$got" == "$want"* ]] || { echo "   got: $got"; return 1; }; }
refresh_csrf() { CSRF="$(get "${1:-/account}" | grep -oE 'name="csrf" value="[^"]+"' | head -n1 | sed 's/.*value="//; s/"$//')"; [ -n "$CSRF" ]; }
# Capture first: `curl | grep -q` fails under pipefail when grep exits early.
page_has()   { local b; b="$(get "$1")" || return 1; grep -qF -- "$2" <<<"$b"; }
page_lacks() { local b; b="$(get "$1")" || return 1; ! grep -qF -- "$2" <<<"$b"; }
flash_has()  { local b; b="$(get "${2:-/account}")" || return 1; grep -qF -- "$1" <<<"$b"; }
# login <password> [email] [jar]: sign in on the current dashboard address; prints the HTTP code.
login() {
  curl -sS -m 30 "${DOPTS[@]}" -b "${3:-$JAR}" -c "${3:-$JAR}" -o /dev/null -w '%{http_code}' -H "Origin: $DASH" \
    --data-urlencode "email=${2-$ADMIN}" --data-urlencode "password=$1" "$DASH/login"
}
signed_in() { rm -f "$JAR"; [ "$(login "${1:-$PW}")" = 303 ] && refresh_csrf /; }

mock_py() { curl -fsS http://127.0.0.1:8787/_mock/state | python3 -c "import json,sys; s=json.load(sys.stdin); $1"; }
dns_is()  { mock_py "import sys; sys.exit(0 if any(r['name']=='$1' and r['type']=='A' and r['content']=='${2:-$PUB_IP}' and r['proxied'] for z in s['zones'].values() for r in z['dns'].values()) else 1)"; }
db()      { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); r=c.execute(sys.argv[1]).fetchone(); print('' if r is None else (r[0].decode() if isinstance(r[0], bytes) else r[0]))" "$1"; }

# ------------------------------------------------------------ apps

# Apps come from GitHub releases. The fake GitHub API (e2etool ghmock)
# serves $GHD/e2e/<repo>/<NNN>-<tag>/<assets>; a higher NNN is newer.
GHD="$E2E/github"
GH_TOKEN="e2e-gh-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"

# mkrepo <name> [private]: an empty GitHub repository e2e/<name> (the app
# will be named <name>).
mkrepo() {
  mkdir -p "$GHD/e2e/$1"
  if [ "${2:-}" = private ]; then touch "$GHD/e2e/$1/private"; fi
}
# publish <repo> <tag> <dir> [name]: a GitHub release with the files in <dir>.
publish() {
  local d="$GHD/e2e/$1" n
  n=$(( $(find "$d" -mindepth 1 -maxdepth 1 -type d | wc -l) + 1 ))
  local r="$d/$(printf '%03d' "$n")-$2"
  mkdir -p "$r"
  cp -r "$3"/. "$r/"
  if [ -n "${4:-}" ]; then printf '%s\n' "$4" > "$r/name"; fi
  RELDIR="$r"
}
# sample_dist <sample-c|sample-zig>: run the sample's own release workflow
# (tests, both architectures, package, smoke test) once; prints the dir.
sample_dist() {
  local out="$E2E/dist-$1"
  if [ ! -f "$out/checksums.txt" ]; then
    mkdir -p "$BIN/runner"
    RUNNER_TEMP="$BIN/runner" python3 scripts/release-workflow.py "examples/$1" --publish-to "$out" >&2 || return 1
  fi
  echo "$out"
}
# echo_dist <dir> [echo args]: release files for the e2etool echo app.
echo_dist() {
  local d="$1"; shift
  mkdir -p "$d"
  for arch in amd64 arm64; do
    local p; p="$(mktemp -d)"; chmod 755 "$p"
    printf 'contract = 2\nrun = "echo-app echo %s"\nhealth_path = "/healthz"\n' "$*" > "$p/dootd.toml"
    install -m 0755 "$BIN/echo-$arch" "$p/echo-app"
    tar -czf "$d/app-linux-$arch.tar.gz" -C "$p" .
    rm -rf "$p"
  done
  (cd "$d" && sha256sum app-linux-*.tar.gz > checksums.txt)
}
# resum <dir>: rewrite checksums.txt after changing a tarball.
resum() { (cd "$1" && sha256sum app-linux-*.tar.gz > checksums.txt); }
# mkecho <name> [echo args]: repo e2e/<name> with release v1 of the echo app.
mkecho() { local n="$1"; shift; mkrepo "$n"; echo_dist "$E2E/echo-$n" "$@"; publish "$n" v1 "$E2E/echo-$n"; }
# create_app <repo name> [--data ...]: Add app; prints "code location".
create_app() {
  local name="$1"; shift
  post /apps --data-urlencode "repo=https://github.com/e2e/$name" "$@"
}
# deploy <app> <tag> / rollback <app> <kept release>: start a deployment and
# follow its live log into $LAST; sets DEP and STATUS (the final status, or
# "refused"). Not meant for $(...): the variables must reach the caller.
N=0
_follow() {
  local loc="$1"
  N=$((N + 1)); LAST="$OUT/deploy-$N.txt"; DEP=""; STATUS=""
  case "$loc" in
    *"/deployments/"*) DEP="${loc##*/deployments/}" ;;
    *) STATUS="refused"; : > "$LAST"; return 0 ;;
  esac
  dc -N --max-time 900 "$DASH/deployments/$DEP/stream" > "$LAST" || true
  STATUS="$(grep -A1 '^event: done' "$LAST" | tail -n1 | sed 's/^data: //')"
}
deploy()   { _follow "$(post "/apps/$1/deploy" --data-urlencode "tag=$2")"; }
rollback() { _follow "$(post "/apps/$1/rollback" --data-urlencode "release=$2")"; }
_is() { local want="$1"; shift; "$@"; [ "$STATUS" = "$want" ] || { echo "   $*: $STATUS"; tail -n 30 "$LAST" | sed 's/^/   | /'; return 1; }; }
deploy_is()   { local want="$1"; shift; _is "$want" deploy "$@"; }
rollback_is() { local want="$1"; shift; _is "$want" rollback "$@"; }
refused()     { deploy "$@"; [ "$STATUS" = refused ]; }
last_has() { grep -qF -- "$1" "$LAST"; }
# port_of <app>: the app's local port (shown on its page).
port_of() { get "/apps/$1" | grep -oE 'port [0-9]+' | head -n1 | awk '{print $2}'; }
current() { basename "$(readlink "$DATA_ROOT/apps/$1/current")"; }

# ------------------------------------------------------------ setup

# e2e_prepare [cfmock args]: clean host, build, local release, fake Cloudflare.
e2e_prepare() {
  [ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
  echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME"), $(nproc) CPUs"
  say "Build dootd, a local release and the e2e tool"
  for u in "$UNIT" cfmock ghmock s3mock relsrv; do systemctl stop "$u" 2>/dev/null || true; done
  for u in $(getent passwd | cut -d: -f1 | grep '^dootd-' || true); do userdel "$u" 2>/dev/null || true; done
  rm -rf "$E2E" "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service /etc/systemd/system/dootd.service.d /usr/local/bin/dootd
  systemctl daemon-reload
  mkdir -p "$MOCK" "$OUT" "$REL" "$GHD" "$BIN"
  chmod 0755 "$E2E" "$BIN"
  make build
  install -m 0755 dist/dootd "$REL/dootd-linux-$ARCH"
  (cd "$REL" && sha256sum "dootd-linux-$ARCH" > checksums.txt)
  go build -o "$BIN/e2etool" ./scripts/e2e/e2etool
  CGO_ENABLED=0 GOARCH=amd64 go build -o "$BIN/echo-amd64" ./scripts/e2e/e2etool
  CGO_ENABLED=0 GOARCH=arm64 go build -o "$BIN/echo-arm64" ./scripts/e2e/e2etool
  chmod 0755 "$BIN/e2etool"
  python3 -c 'import yaml' 2>/dev/null || apt-get install -y -qq python3-yaml >/dev/null
  systemd-run --unit=ghmock --collect -q "$BIN/e2etool" ghmock -listen 127.0.0.1:8902 -dir "$GHD" -token "$GH_TOKEN"
  ip addr add "$CF_IP/32" dev lo 2>/dev/null || true
  systemd-run --unit=relsrv --collect -q python3 -m http.server 8900 --bind 127.0.0.1 --directory "$REL"
  systemd-run --unit=cfmock --collect -q "$BIN/e2etool" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
    -token "$TOKEN" -extra-range 198.18.0.0/15 "${@:--zones=example.test}"
  wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state
  wait_for 10 curl -fsS http://127.0.0.1:8900/checksums.txt
  wait_for 10 curl -fsS -o /dev/null http://127.0.0.1:8902/user/repos
  test_env
}

# test_env [NAME=value ...]: the DOOTD_TEST_* drop-in (applied at the next start).
test_env() {
  mkdir -p "$(dirname "$DROPIN")"
  {
    echo "[Service]"
    echo "Environment=DOOTD_TEST_CLOUDFLARE_API=http://127.0.0.1:8787/client/v4"
    echo "Environment=DOOTD_TEST_GITHUB_API=http://127.0.0.1:8902"
    echo "Environment=DOOTD_TEST_PUBLIC_IPV4=$PUB_IP"
    echo "Environment=DOOTD_TEST_PUBLIC_IPV6=off"
    echo "Environment=DOOTD_TEST_AOP_ROLLOUT=0s"
    for kv in "$@"; do echo "Environment=DOOTD_TEST_$kv"; done
  } > "$DROPIN"
  systemctl daemon-reload
}

# e2e_install [release subdir]: run install.sh from the local release; sets OTP.
NINST=0
e2e_install() {
  NINST=$((NINST + 1)); INSTALL_OUT="$OUT/install-$NINST.txt"
  DOOTD_BASE_URL="http://127.0.0.1:8900/${1:-}" DOOTD_TEST_PUBLIC_IPV4="$PUB_IP" bash ./install.sh 2>&1 | tee "$INSTALL_OUT"
  OTP="$(grep -oE 'One-time password: +[a-z0-9-]+' "$INSTALL_OUT" | awk '{print $3}')"
  [ -n "$OTP" ]
}

setup_closed() { ! curl -sS -k -m 5 -o /dev/null https://127.0.0.1/ 2>/dev/null; }

# e2e_setup: install, then do the first-run steps in the browser: one-time
# password → account → Cloudflare token → dashboard domain; ends signed in
# on the dashboard domain.
e2e_setup() {
  say "Install and first sign-in"
  e2e_install "${1:-}"
  wait_for 30 curl -sk -o /dev/null https://127.0.0.1/login
  use_setup; rm -f "$JAR"
  [ "$(login "$OTP" "")" = 303 ] || { echo "one-time password sign-in failed"; return 1; }
  refresh_csrf /setup
  post /setup --data-urlencode "email=$ADMIN" --data-urlencode "password=$PW" --data-urlencode "confirm=$PW" >/dev/null
  refresh_csrf /settings
  post /settings/cloudflare-token --data-urlencode "token=$TOKEN" >/dev/null
  post /settings/dashboard-domain --data "domain=$D" >/dev/null
  wait_for 120 setup_closed || { echo "setup address did not close"; return 1; }
  use_domain
  wait_for 30 signed_in || { echo "sign-in on $D failed"; return 1; }
}

e2e_result() {
  say "Result"
  systemctl stop "$UNIT" 2>/dev/null || true
  if [ "$FAILS" -gt 0 ]; then
    echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 200 || true
    echo "---- cfmock ----"; journalctl -u cfmock --no-pager -n 20 || true
    echo "---- ghmock ----"; journalctl -u ghmock --no-pager -n 20 || true
    echo "$FAILS check(s) failed"; exit 1
  fi
  echo "all checks passed"
}
