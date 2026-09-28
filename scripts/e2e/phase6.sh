#!/usr/bin/env bash
# Phase 6 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# Five apps (the e2etool echo app, prebuilt) run under dootd with the edge,
# the dashboard and a fake Cloudflare API. Checks: samples for the server,
# dootd and every app; CPU numbers against the kernel's own counters (one
# unthrottled and one 0.5-core app burning CPU); memory against
# memory.current; request, 5xx and latency stats; 1-minute rollups that
# survive a restart; 7-day pruning; SVG charts and pages; the disk,
# memory, OOM and certificate warnings; and dootd's own budget (< 30 MB
# resident, < 1% CPU while idle with 5 apps; Req 1.3).
#
#   sudo ./scripts/e2e/phase6.sh
set -euo pipefail

UNIT="dootd"
DATA_ROOT="/var/lib/dootd"
E2E="/opt/dootd-e2e6"
MOCK="$E2E/mock"
OUT="$E2E/out"
CF_IP="198.18.0.10"
D="dootd.example.test"
APP_HOST="one.example.test"
TOKEN="e2e-cf-token-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PW="phase six password"
CG="/sys/fs/cgroup/system.slice/dootd.service/apps"
FAILS=0

say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
check() { local desc="$1"; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
wait_for() { local t="$1"; shift; for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done; return 1; }

TLS=(--resolve "$D:443:$CF_IP" --resolve "$APP_HOST:443:$CF_IP" --cacert "$MOCK/origin-ca.pem" --cert "$MOCK/aop-client.pem" --key "$MOCK/aop-client.key")
JAR="$OUT/jar.txt"
dc()        { curl -sS -m 60 "${TLS[@]}" -b "$JAR" -c "$JAR" "$@"; }
get()       { dc "https://$D$1"; }
page_has()  { local b; b="$(get "$1")" || return 1; grep -qF -- "$2" <<<"$b"; }
page_lacks() { local b; b="$(get "$1")" || return 1; ! grep -qF -- "$2" <<<"$b"; }
app()       { curl -sS -m 10 "${TLS[@]}" "https://$APP_HOST$1"; }
local_app() { curl -sS -m 30 "http://127.0.0.1:$1$2"; }
# m <scope> <field>: latest value from the control API.
m() { curl -sS --unix-socket /run/dootd/dootd.sock http://d/v1/metrics |
  python3 -c "import json,sys; d={x['scope']:x for x in json.load(sys.stdin)}; v=d.get(sys.argv[1],{}).get(sys.argv[2]); print('' if v is None else v)" "$1" "$2"; }
between() { python3 -c "import sys; v,lo,hi=map(float,sys.argv[1:]); sys.exit(0 if lo<=v<=hi else 1)" "$1" "$2" "$3"; }
has_scope() { [ -n "$(m "$1" mem)" ]; }
rows()      { python3 -c "import sqlite3,sys; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); print(c.execute('SELECT count(*) FROM metrics_1m WHERE scope=?',(sys.argv[1],)).fetchone()[0])" "$1"; }
has_rows()  { [ "$(rows "$1")" -ge "$2" ]; }
# kernel_cpu <app> <seconds>: CPU % of one core measured from cpu.stat.
kernel_cpu() {
  local a b; a="$(awk '/^usage_usec/ {print $2}' "$CG/$1/cpu.stat")"; sleep "$2"
  b="$(awk '/^usage_usec/ {print $2}' "$CG/$1/cpu.stat")"
  python3 -c "print(($b-$a)/1e6/$2*100)"
}
apps_have_rows() { for a in one half mem oom idle; do has_rows "$a" 1 || return 1; done; }
no_old_rows() { [ "$(python3 -c "import sqlite3,time; c=sqlite3.connect('file:$DATA_ROOT/dootd.db?mode=ro',uri=True); print(c.execute('SELECT count(*) FROM metrics_1m WHERE ts < ?',(int(time.time())-7*86400,)).fetchone()[0])")" = 0 ]; }
lists_apps() { local b; b="$(get /metrics)" || return 1; for a in one half mem oom idle; do grep -qF "href=\"/apps/$a#usage\"" <<<"$b" || return 1; done; }
four_charts() { local b; b="$(get /apps/one)" || return 1; [ "$(grep -o '<img src="/charts?scope=one' <<<"$b" | wc -l)" = 4 ]; }
svg_ok() { # svg_ok <query> <min paths>: valid SVG with at least N data lines
  local f="$OUT/chart.svg" ct
  ct="$(dc -o "$f" -w '%{content_type}' "https://$D/charts?$1")" || return 1
  [[ "$ct" == image/svg+xml* ]] || return 1
  python3 - "$f" "$2" <<'PY'
import sys, xml.etree.ElementTree as ET
r = ET.parse(sys.argv[1]).getroot()
paths = [p for p in r.iter('{http://www.w3.org/2000/svg}path') if not p.get('stroke-dasharray')]
sys.exit(0 if len(paths) >= int(sys.argv[2]) else 1)
PY
}

[ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }
cd "$(dirname "$0")/../.."
echo "kernel $(uname -r), $(. /etc/os-release && echo "$PRETTY_NAME"), $(nproc) CPUs"

say "Build and install"
make build
mkdir -p "$E2E-bin"
go build -o "$E2E-bin/e2etool" ./scripts/e2e/e2etool
for u in "$UNIT" cfmock; do systemctl stop "$u" 2>/dev/null || true; done
install -m 0755 dist/dootd /usr/local/bin/dootd
rm -rf "$E2E" "$DATA_ROOT" /etc/dootd /etc/systemd/system/dootd.service.d
mkdir -p "$MOCK" "$OUT" "$E2E/echo" /etc/dootd /etc/systemd/system/dootd.service.d
install -m 0755 "$E2E-bin/e2etool" "$E2E/echo/echo-app"
chmod 0755 "$E2E" "$E2E/echo"
ip addr add "$CF_IP/32" dev lo 2>/dev/null || true
systemd-run --unit=cfmock --collect -q "$E2E-bin/e2etool" cfmock -listen 127.0.0.1:8787 -dir "$MOCK" \
  -token "$TOKEN" -zones example.test -extra-range 198.18.0.0/15 -short-first
wait_for 10 curl -fsS http://127.0.0.1:8787/_mock/state

cat > /etc/dootd/config.toml <<EOF
[edge]
dashboard_domain = "$D"
public_ipv4      = "203.0.113.10"
public_ipv6      = "off"
cloudflare_api   = "http://127.0.0.1:8787/client/v4"
EOF
{
  i=0
  for spec in "one:1:256M:$APP_HOST" "half:0.5:256M:" "mem:1:256M:" "oom:1:64M:" "idle:1:256M:"; do
    IFS=: read -r name cpu memlim domain <<<"$spec"; i=$((i + 1))
    printf '[[app]]\nname = "%s"\ntype = "c"\nport = %d\nrelease_dir = "%s"\nrun = "echo-app echo"\nhealth_path = "/healthz"\ncpu = %s\nmemory = "%s"\n' \
      "$name" $((20060 + i)) "$E2E/echo" "$cpu" "$memlim"
    [ -n "$domain" ] && printf 'domain = "%s"\n' "$domain"
    echo
  done
} > /etc/dootd/dev-apps.toml
install -m 0644 contrib/systemd/dootd.service /etc/systemd/system/dootd.service
cat > /etc/systemd/system/dootd.service.d/dev.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/dootd serve --dev-apps /etc/dootd/dev-apps.toml
EOF
systemctl daemon-reload
systemctl start "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
for p in 20061 20062 20063 20064 20065; do wait_for 30 curl -fsS "http://127.0.0.1:$p/healthz"; done
printf '%s' "$TOKEN" | dootd ctl cloudflare-token >/dev/null
dootd ctl edge sync >/dev/null 2>&1 || true
wait_for 60 bash -c "dootd ctl edge | grep -Eq '^example.test .* enforced'"
printf '%s' "$PW" | dootd ctl admin set-password --email admin@example.test >/dev/null
curl -sS "${TLS[@]}" -c "$JAR" -o /dev/null -H "Origin: https://$D" --data-urlencode email=admin@example.test --data-urlencode "password=$PW" "https://$D/login"

say "Samples for the server, dootd and every app"
check "server sampled" wait_for 30 has_scope _host
for s in _dootd one half mem oom idle; do check "$s sampled" wait_for 30 has_scope "$s"; done
dootd ctl top | tee "$OUT/top.txt"
check "ctl top lists the server, dootd and 5 apps" test "$(grep -cE '^(\(server\)|\(dootd\)|one|half|mem|oom|idle) ' "$OUT/top.txt")" -eq 7
check "server memory total matches /proc/meminfo" test "$(m _host mem_limit)" = "$(( $(awk '/^MemTotal/ {print $2}' /proc/meminfo) * 1024 ))"
check "app memory limit reported (oom: 64M)" test "$(m oom mem_limit)" = 67108864
check "certificate warning while the first (10-day) certificate is in use" page_has / "expires in"
dootd ctl edge sync >/dev/null 2>&1 || true
check "renewed certificate clears the warning" page_lacks / "and has not been renewed"

say "CPU: dootd's numbers against the kernel's counters"
local_app 20061 "/burn?s=40" >/dev/null
local_app 20062 "/burn?s=40" >/dev/null
sleep 12
K1="$(kernel_cpu one 10)"; D1="$(m one cpu)"
K2="$(kernel_cpu half 10)"; D2="$(m half cpu)"
echo "  one: kernel ${K1}% dootd ${D1}%   half: kernel ${K2}% dootd ${D2}%"
check "unthrottled burner ~100% of a core" between "$D1" 80 110
check "0.5-core burner held at ~50%" between "$D2" 40 60
check "dootd within 15 points of the kernel (one)" between "$(python3 -c "print(abs($D1-$K1))")" 0 15
check "dootd within 15 points of the kernel (half)" between "$(python3 -c "print(abs($D2-$K2))")" 0 15
check "server CPU reflects the load" between "$(m _host cpu)" "$(python3 -c "print(100*1.2/$(nproc)*0.7)")" 100
check "idle app near 0%" between "$(m idle cpu)" 0 5

say "Memory against memory.current"
local_app 20063 "/alloc?mb=60" >/dev/null
sleep 11
DM="$(m mem mem)"; KM="$(cat "$CG/mem/memory.current")"
echo "  mem: dootd $((DM >> 20)) MB, memory.current $((KM >> 20)) MB"
check "at least 60 MB reported" test "$DM" -ge $((60 << 20))
check "within 10% of memory.current" between "$(python3 -c "print(abs($DM-$KM)/$KM)")" 0 0.10

say "Requests, errors and latency"
for _ in $(seq 1 40); do app / >/dev/null; done
for _ in $(seq 1 10); do app /fail >/dev/null || true; done
sleep 11
check "requests/min reported" between "$(m one req)" 1 100000
check "5xx/min reported" between "$(m one req_5xx)" 1 100000
check "p95 latency reported" test -n "$(m one p95_ms)"
check "apps without traffic report 0 requests" test "$(m half req)" = 0

say "Warnings"
local_app 20064 "/alloc?mb=100" >/dev/null 2>&1 || true
check "OOM kill shows a warning with the limit" wait_for 30 page_has / "oom was killed 1 time(s) in the last hour for using more than its 64 MB memory limit"
check "no disk warning at the default 85%" page_lacks / "Disk is"
cat >> /etc/dootd/config.toml <<'EOF'

[monitoring]
disk_warn_percent   = 1
memory_warn_percent = 1
EOF
systemctl restart "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
for p in 20061 20062 20063 20064 20065; do wait_for 30 curl -fsS "http://127.0.0.1:$p/healthz"; done
check "disk warning at a 1% threshold" wait_for 30 page_has / "Disk is"
check "memory warning at a 1% threshold" page_has / "Memory is"

say "Rollups, restart and pruning"
python3 - "$DATA_ROOT/dootd.db" <<'PY'
import sqlite3, sys, time
c = sqlite3.connect(sys.argv[1])
c.execute("INSERT INTO metrics_1m VALUES ('one', ?, 1,1,1,0,0,0,0,1,0,0,0,0,NULL,NULL,0)", (int(time.time()) - 8*86400,))
c.commit()
PY
check "1-minute rollups written for the server" wait_for 75 has_rows _host 1
check "... for dootd" has_rows _dootd 1
check "... for each app" apps_have_rows
systemctl restart "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
check "rows older than 7 days pruned at startup" wait_for 10 no_old_rows
check "24h chart still has data right after a restart" svg_ok "scope=one&chart=cpu&range=24h" 1

say "Charts and pages"
sleep 11
check "server CPU chart (1h)" svg_ok "scope=_host&chart=cpu&range=1h" 1
check "server memory chart has used + swap lines" svg_ok "scope=_host&chart=memory&range=1h" 2
check "server load chart (7d)" svg_ok "scope=_host&chart=load&range=7d" 1
check "server disk chart (24h)" svg_ok "scope=_host&chart=disk&range=24h" 1
check "dootd memory chart" svg_ok "scope=_dootd&chart=memory&range=1h" 1
check "app requests chart has requests + 5xx" svg_ok "scope=one&chart=requests&range=24h" 2
check "app latency chart" svg_ok "scope=one&chart=latency&range=24h" 1
check "unknown scope: 404" test "$(dc -o /dev/null -w '%{http_code}' "https://$D/charts?scope=nope&chart=cpu")" = 404
check "unknown chart: 404" test "$(dc -o /dev/null -w '%{http_code}' "https://$D/charts?scope=one&chart=nope")" = 404
check "charts need a session" test "$(curl -sS "${TLS[@]}" -o /dev/null -w '%{http_code}' "https://$D/charts?scope=_host&chart=cpu")" = 303
check "monitoring page lists every app" lists_apps
check "monitoring page embeds the charts" page_has /metrics '<img src="/charts?scope=_host&amp;chart=cpu&amp;range=1h'
check "app page has a usage section with 4 charts" four_charts
check "range links switch the charts" page_has "/apps/one?range=7d" "range=7d&amp;t="

say "dootd's own budget: idle with 5 apps (Req 1.3)"
systemctl restart "$UNIT"
wait_for 10 test -S /run/dootd/dootd.sock
for p in 20061 20062 20063 20064 20065; do wait_for 30 curl -fsS "http://127.0.0.1:$p/healthz"; done
sleep 30
PID="$(systemctl show -p MainPID --value "$UNIT")"
T0="$(awk '{print $14+$15}' "/proc/$PID/stat")"
sleep 60
T1="$(awk '{print $14+$15}' "/proc/$PID/stat")"
RSS="$(awk '/^VmRSS/ {print $2*1024}' "/proc/$PID/status")"
CPU="$(python3 -c "print(($T1-$T0)/100/60*100)")"
echo "  dootd: $((RSS >> 20)) MB resident, ${CPU}% CPU over 60 s"
check "resident memory under 30 MB" test "$RSS" -lt $((30 << 20))
check "CPU under 1% while idle" between "$CPU" 0 1
check "dootd's self-report matches /proc (within 20%)" between "$(python3 -c "print(abs($(m _dootd mem)-$RSS)/$RSS)")" 0 0.2
systemctl stop "$UNIT"

say "Result"
if [ "$FAILS" -gt 0 ]; then
  echo "---- journal ----"; journalctl -u "$UNIT" --no-pager -n 120 || true
  echo "$FAILS check(s) failed"; exit 1
fi
echo "all checks passed"
