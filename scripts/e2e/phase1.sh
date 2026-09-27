#!/usr/bin/env bash
# Phase 1 end-to-end check on a real Ubuntu 24.04 host (GitHub Actions runner
# or a test VPS). Must run as root. Builds dootd and both sample apps, runs
# dootd as the real systemd unit with a dev-apps drop-in, and verifies:
#   per-app users, cgroup placement + limits, clean env, DATA_DIR ownership,
#   health checks, OOM detection + restart, crash backoff -> crashed,
#   graceful stop with no leftover processes, data persisting across restarts.
#
#   sudo ./scripts/e2e/phase1.sh            (from the repo root)
set -euo pipefail

ZIG_VERSION="0.16.0"
DEV_ROOT="/opt/dootd-dev"
DATA_ROOT="/var/lib/dootd"
UNIT="dootd"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }

wait_for() { # wait_for <seconds> <cmd...>
  local t="$1"; shift
  for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done
  return 1
}
healthy() { curl -fsS -m 2 "http://127.0.0.1:$1/healthz" >/dev/null; }
app_pid() { pgrep -u "dootd-$1" -x "$1" | head -n1; }
applog()  { cat "$DATA_ROOT/apps/$1/logs/app.log"; }
log_has() { grep -qF -- "$2" "$DATA_ROOT/apps/$1/logs/app.log"; }
cg()      { cat "/sys/fs/cgroup/system.slice/${UNIT}.service/apps/$1/$2"; }
envof()   { tr '\0' '\n' < "/proc/$1/environ"; }
env_has() { envof "$1" | grep -x -- "$2" >/dev/null; }
env_lacks() { ! envof "$1" | grep -E -- "$2" >/dev/null; }
no_procs() { for u in "$@"; do pgrep -u "dootd-$u" >/dev/null && return 1; done; return 0; }
backoffs_logged() { for d in 1s 2s 4s 8s; do log_has sample-zig "restarting in $d" || return 1; done; }
cannot_read_other() { ! sudo -u dootd-sample-zig ls "$DATA_ROOT/apps/sample-c/data" >/dev/null 2>&1; }
down() { ! healthy "$1"; }

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."

say "Install Zig $ZIG_VERSION"
if ! command -v zig >/dev/null || [ "$(zig version)" != "$ZIG_VERSION" ]; then
  arch="$(uname -m)"
  url="$(curl -fsSL https://ziglang.org/download/index.json |
    python3 -c "import json,sys; e=json.load(sys.stdin)['$ZIG_VERSION']['${arch}-linux']; print(e['tarball'], e['shasum'])")"
  set -- $url
  curl -fsSL -o /tmp/zig.tar.xz "$1"
  echo "$2  /tmp/zig.tar.xz" | sha256sum -c -
  mkdir -p /opt/zig && tar -xJf /tmp/zig.tar.xz -C /opt/zig --strip-components=1
  ln -sf /opt/zig/zig /usr/local/bin/zig
fi
zig version

say "Build dootd and sample apps"
make build
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$DEV_ROOT" && mkdir -p "$DEV_ROOT"
cp -r examples/sample-zig examples/sample-c "$DEV_ROOT/"
(cd "$DEV_ROOT/sample-zig" && zig build -Doptimize=ReleaseSafe)
(cd "$DEV_ROOT/sample-c" && make CC="zig cc")
chmod -R a+rX "$DEV_ROOT"

say "Install systemd unit with dev-apps drop-in"
mkdir -p /etc/dootd /etc/systemd/system/dootd.service.d
cat > /etc/dootd/dev-apps.toml <<EOF
[[app]]
name        = "sample-zig"
type        = "zig"
port        = 20001
release_dir = "$DEV_ROOT/sample-zig"
run         = "zig-out/bin/sample-zig"
health_path = "/healthz"
memory      = "64M"

[[app]]
name        = "sample-c"
type        = "c"
port        = 20002
release_dir = "$DEV_ROOT/sample-c"
run         = "build/sample-c"
health_path = "/healthz"
memory      = "64M"
cpu         = 0.5
[app.env]
GREETING = "hello"
EOF
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
cat > /etc/systemd/system/dootd.service.d/dev.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/dootd serve --dev-apps /etc/dootd/dev-apps.toml
EOF
systemctl daemon-reload
systemctl restart "$UNIT"

say "Apps start and become healthy"
check "sample-zig healthy" wait_for 30 healthy 20001
check "sample-c healthy"   wait_for 30 healthy 20002
curl -fsS http://127.0.0.1:20002/ >/dev/null
curl -fsS http://127.0.0.1:20002/ >/dev/null

say "Isolation: users, cgroups, limits, env, directories"
for a in sample-zig sample-c; do
  pid="$(app_pid "$a")"
  check "$a runs as dootd-$a" [ "$(ps -o user= -p "$pid" | tr -d ' ')" = "dootd-$a" ]
  check "$a is in apps/$a cgroup" grep -qx "0::/system.slice/${UNIT}.service/apps/$a" "/proc/$pid/cgroup"
  check "$a memory.max = 64M" [ "$(cg "$a" memory.max)" = "67108864" ]
  check "$a memory.swap.max = 0" [ "$(cg "$a" memory.swap.max)" = "0" ]
  check "$a pids.max = 256" [ "$(cg "$a" pids.max)" = "256" ]
  check "$a DATA_DIR owned by app user, 0700" [ "$(stat -c '%U %a' "$DATA_ROOT/apps/$a/data")" = "dootd-$a 700" ]
  check "$a open-files limit 4096" grep -Eq '^Max open files +4096 +4096' "/proc/$pid/limits"
  check "$a env DATA_DIR" env_has "$pid" "DATA_DIR=$DATA_ROOT/apps/$a/data"
  check "$a env HOST" env_has "$pid" "HOST=127.0.0.1"
  check "$a env DOOTD_CONTRACT" env_has "$pid" "DOOTD_CONTRACT=1"
  check "$a env does not inherit dootd's env" env_lacks "$pid" '^(INVOCATION_ID|JOURNAL_STREAM)='
done
check "sample-c cpu.max = 0.5 core" [ "$(cg sample-c cpu.max)" = "50000 100000" ]
check "sample-c got user env" env_has "$(app_pid sample-c)" "GREETING=hello"
check "sample-c env PORT" env_has "$(app_pid sample-c)" "PORT=20002"
check "sample-c created its DB in DATA_DIR" [ -f "$DATA_ROOT/apps/sample-c/data/app.db" ]
check "other apps cannot read this app's data" cannot_read_other
check "app logs captured with timestamps" grep -Eq '^[0-9T:.-]+Z out +GET /$' "$DATA_ROOT/apps/sample-c/logs/app.log"

say "OOM: exceeding memory.max is detected and the app restarts"
old="$(app_pid sample-c)"
curl -s -m 10 "http://127.0.0.1:20002/alloc?mb=200" >/dev/null || true
check "OOM kill logged" wait_for 10 log_has sample-c "OOM:"
check "sample-c restarted and healthy" wait_for 30 healthy 20002
check "sample-c has a new pid" [ "$(app_pid sample-c)" != "$old" ]

say "Crash loop: backoff, then crashed after 5 failures"
for i in 1 2 3 4 5; do
  wait_for 30 healthy 20001 || { fail "sample-zig not healthy before crash $i"; break; }
  curl -s -m 2 http://127.0.0.1:20001/crash >/dev/null || true
  wait_for 5 down 20001
done
check "backoff delays logged (1s, 2s, 4s, 8s)" backoffs_logged
check "marked crashed" wait_for 5 log_has sample-zig "crashed: 5 failures"
sleep 3
check "no restart after crashed" no_procs sample-zig
check "sample-c unaffected" healthy 20002

say "Graceful stop leaves no processes"
systemctl stop "$UNIT"
check "sample-c got SIGTERM and shut down cleanly" log_has sample-c "SIGTERM received, shutting down"
check "sample-c exited with code 0" log_has sample-c "exited: exit code 0"
check "no app processes left" no_procs sample-c sample-zig

say "Restart: data and log history persist"
systemctl start "$UNIT"
check "sample-c healthy again" wait_for 30 healthy 20002
check "sample-zig starts again after dootd restart" wait_for 30 healthy 20001
check "visit counter persisted (3rd visit)" bash -c "curl -fsS http://127.0.0.1:20002/ | grep -q 'Visits: 3<'"
systemctl kill -s USR1 --kill-whom=main "$UNIT"; sleep 1
journalctl -u "$UNIT" --no-pager -n 5 | grep 'msg=status' || true
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 200 || true
  for a in sample-zig sample-c; do echo "---- $a app.log ----"; applog "$a" | tail -n 60 || true; done
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
