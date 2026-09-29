#!/usr/bin/env bash
# Phase 1 end-to-end check on a real Ubuntu 24.04 host (GitHub Actions runner
# or a test VPS). Must run as root. Installs dootd with install.sh, sets it
# up in the dashboard, deploys both sample apps from local git repos and
# verifies: per-app users, cgroup placement + limits, clean env, DATA_DIR
# ownership, health checks, OOM detection + restart, crash backoff ->
# crashed, graceful stop with no leftover processes, data persisting across
# restarts.
#
#   sudo ./scripts/e2e/phase1.sh            (from the repo root)
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

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

e2e_prepare
e2e_setup

say "Add and deploy both sample apps"
mkrepo sample-zig examples/sample-zig
mkrepo sample-c examples/sample-c
check "sample-zig created" post_is "303 $DASH/apps/sample-zig" /apps --data type=zig --data-urlencode "repo=file://$GIT/sample-zig.git" \
  --data branch=main --data memory=64M
check "sample-c created" post_is "303 $DASH/apps/sample-c" /apps --data type=c --data-urlencode "repo=file://$GIT/sample-c.git" \
  --data branch=main --data memory=64M --data cpu=0.5
post /apps/sample-c/env --data key=GREETING --data value=hello >/dev/null
check "deploy sample-zig" deploy_is succeeded sample-zig
check "deploy sample-c" deploy_is succeeded sample-c
PZ="$(port_of sample-zig)"; PC="$(port_of sample-c)"
echo "  ports: sample-zig $PZ, sample-c $PC"

say "Apps start and become healthy"
check "sample-zig healthy" wait_for 30 healthy "$PZ"
check "sample-c healthy"   wait_for 30 healthy "$PC"
curl -fsS "http://127.0.0.1:$PC/" >/dev/null
curl -fsS "http://127.0.0.1:$PC/" >/dev/null

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
  check "$a env DOOTD_APP (from the repo name)" env_has "$pid" "DOOTD_APP=$a"
  check "$a env DOOTD_CONTRACT" env_has "$pid" "DOOTD_CONTRACT=1"
  check "$a env does not inherit dootd's env" env_lacks "$pid" '^(INVOCATION_ID|JOURNAL_STREAM|DOOTD_TEST_)'
done
check "sample-c cpu.max = 0.5 core" [ "$(cg sample-c cpu.max)" = "50000 100000" ]
check "sample-c got user env" env_has "$(app_pid sample-c)" "GREETING=hello"
check "sample-c env PORT" env_has "$(app_pid sample-c)" "PORT=$PC"
check "sample-c created its DB in DATA_DIR" [ -f "$DATA_ROOT/apps/sample-c/data/app.db" ]
check "other apps cannot read this app's data" cannot_read_other
check "app logs captured with timestamps" grep -Eq '^[0-9T:.-]+Z out +GET /$' "$DATA_ROOT/apps/sample-c/logs/app.log"

say "OOM: exceeding memory.max is detected and the app restarts"
old="$(app_pid sample-c)"
curl -s -m 10 "http://127.0.0.1:$PC/alloc?mb=200" >/dev/null || true
check "OOM kill logged" wait_for 10 log_has sample-c "OOM:"
check "sample-c restarted and healthy" wait_for 30 healthy "$PC"
check "sample-c has a new pid" [ "$(app_pid sample-c)" != "$old" ]
check "dashboard shows the OOM kill" page_has /apps/sample-c "out-of-memory kills"

say "Crash loop: backoff, then crashed after 5 failures"
for i in 1 2 3 4 5; do
  wait_for 30 healthy "$PZ" || { fail "sample-zig not healthy before crash $i"; break; }
  curl -s -m 2 "http://127.0.0.1:$PZ/crash" >/dev/null || true
  wait_for 5 down "$PZ"
done
check "backoff delays logged (1s, 2s, 4s, 8s)" backoffs_logged
check "marked crashed" wait_for 5 log_has sample-zig "crashed: 5 failures"
sleep 3
check "no restart after crashed" no_procs sample-zig
check "dashboard warns that it crashed" page_has / "sample-zig has crashed"
check "sample-c unaffected" healthy "$PC"
refresh_csrf /apps/sample-zig
post /apps/sample-zig/start >/dev/null
check "Start in the dashboard brings it back" wait_for 30 healthy "$PZ"

say "Graceful stop leaves no processes"
systemctl stop "$UNIT"
check "sample-c got SIGTERM and shut down cleanly" log_has sample-c "SIGTERM received, shutting down"
check "sample-c exited with code 0" log_has sample-c "exited: exit code 0"
check "no app processes left" no_procs sample-c sample-zig

say "Restart: data and log history persist"
systemctl start "$UNIT"
check "sample-c healthy again" wait_for 30 healthy "$PC"
check "sample-zig healthy again" wait_for 30 healthy "$PZ"
check "visit counter persisted (3rd visit)" bash -c "curl -fsS http://127.0.0.1:$PC/ | grep -q 'Visits: 3<'"
systemctl kill -s USR1 --kill-whom=main "$UNIT"; sleep 1
journalctl -u "$UNIT" --no-pager -n 5 | grep 'msg=status' || true

if [ "$FAILS" -gt 0 ]; then
  for a in sample-zig sample-c; do echo "---- $a app.log ----"; applog "$a" | tail -n 60 || true; done
fi
e2e_result
