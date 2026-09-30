#!/usr/bin/env bash
# Phase 6 end-to-end check on a real Ubuntu 24.04 host. Must run as root.
# Five apps (the e2etool echo app, deployed from git repos) run under dootd
# with the edge, the dashboard and a fake Cloudflare API. The numbers are
# read from the dashboard's Monitoring page and checked against the
# kernel's own counters: CPU (one unthrottled and one 0.5-core app burning
# CPU), memory against memory.current; request, 5xx and latency stats;
# 1-minute rollups that survive a restart; 7-day pruning; SVG charts and
# pages; the disk, memory, OOM and certificate warnings; and dootd's own
# budget (< 30 MB resident, < 1% CPU while idle with 5 apps; Req 1.3).
#
#   sudo ./scripts/e2e/phase6.sh
cd "$(dirname "$0")/../.."
. scripts/e2e/lib.sh

APP_HOST="one.example.test"
CG="/sys/fs/cgroup/system.slice/dootd.service/apps"
APPS="one half mem oom idle"

app()       { site "$APP_HOST" "$1"; }
local_app() { curl -sS -m 30 "http://127.0.0.1:$(port "$1")$2"; }
declare -A PORTS
port()      { echo "${PORTS[$1]}"; }
# m <scope> <field>: the latest value shown on the Monitoring page.
#   apps: cpu mem mem_limit req p95 · _host: cpu mem mem_limit · _dootd: cpu mem
m() {
  get /metrics > "$OUT/metrics.html" || return 1
  python3 - "$OUT/metrics.html" "$1" "$2" <<'PY'
import html, re, sys
page, scope, field = open(sys.argv[1]).read(), sys.argv[2], sys.argv[3]
units = {"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30}
def size(s):
    n, u = s.split()
    return str(int(float(n) * units[u]))
def out(v):
    print(v); sys.exit(0)
if scope == "_host":
    t = re.search(r"Now: CPU ([\d.]+)% · memory ([\d.]+ \w+) of ([\d.]+ \w+)", page)
    if t: out({"cpu": t[1], "mem": size(t[2]), "mem_limit": size(t[3])}[field])
elif scope == "_dootd":
    t = re.search(r"Now: ([\d.]+ \w+) resident \(target under 30 MB\) · CPU ([\d.]+)%", page)
    if t: out({"mem": size(t[1]), "cpu": t[2]}[field])
else:
    row = re.search(r'<tr>\s*<td><a href="/apps/%s#usage">.*?</tr>' % re.escape(scope), page, re.S)
    if row:
        tds = [re.sub(r"<[^>]+>", " ", html.unescape(x)).split() for x in re.findall(r"<td[^>]*>(.*?)</td>", row[0], re.S)]
        if len(tds) >= 8 and tds[2] and tds[2][0].endswith("%"):
            cpu, memory, req, p95 = tds[2][0].rstrip("%"), tds[3], tds[6][0], tds[7][0]
            v = {"cpu": cpu, "mem": size(" ".join(memory[0:2])), "mem_limit": size(" ".join(memory[3:5])),
                 "req": req, "p95": "" if p95 == "-" else p95}[field]
            out(v)
print("")
PY
}
has_scope() { [ -n "$(m "$1" mem)" ]; }
fivexx()    { get /apps/one | grep -oE '\([0-9]+ 5xx\)' | head -n1 | sed -E 's/\(([0-9]+) 5xx\)/\1/'; }
rows()      { db "SELECT count(*) FROM metrics_1m WHERE scope = '$1'"; }
has_rows()  { [ "$(rows "$1")" -ge "$2" ]; }
# kernel_cpu <app> <seconds>: CPU % of one core measured from cpu.stat.
kernel_cpu() {
  local a b; a="$(awk '/^usage_usec/ {print $2}' "$CG/$1/cpu.stat")"; sleep "$2"
  b="$(awk '/^usage_usec/ {print $2}' "$CG/$1/cpu.stat")"
  python3 -c "print(($b-$a)/1e6/$2*100)"
}
apps_have_rows() { for a in $APPS; do has_rows "$a" 1 || return 1; done; }
no_old_rows() { [ "$(db "SELECT count(*) FROM metrics_1m WHERE ts < strftime('%s','now') - 7*86400")" = 0 ]; }
lists_apps() { local b; b="$(get /metrics)" || return 1; for a in $APPS; do grep -qF "href=\"/apps/$a#usage\"" <<<"$b" || return 1; done; }
four_charts() { local b; b="$(get /apps/one)" || return 1; [ "$(grep -o '<img src="/charts?scope=one' <<<"$b" | wc -l)" = 4 ]; }
svg_ok() { # svg_ok <query> <min lines>: valid SVG with at least N data lines
  local f="$OUT/chart.svg" ct
  ct="$(dc -o "$f" -w '%{content_type}' "$DASH/charts?$1")" || return 1
  [[ "$ct" == image/svg+xml* ]] || return 1
  python3 - "$f" "$2" <<'PY'
import sys, xml.etree.ElementTree as ET
r = ET.parse(sys.argv[1]).getroot()
ns = '{http://www.w3.org/2000/svg}'
# A line is a solid path, or circles for points without neighbours.
colors = {p.get('stroke') for p in r.iter(ns + 'path') if not p.get('stroke-dasharray')}
colors |= {c.get('fill') for c in r.iter(ns + 'circle')}
sys.exit(0 if len(colors) >= int(sys.argv[2]) else 1)
PY
}
all_healthy() { for a in $APPS; do curl -fsS -m 2 "http://127.0.0.1:$(port "$a")/healthz" >/dev/null || return 1; done; }

e2e_prepare -zones example.test -short-first
e2e_setup

say "Five apps"
first=1
for spec in "one:1:256M:$APP_HOST" "half:0.5:256M:" "mem:1:256M:" "oom:1:64M:" "idle:1:256M:"; do
  IFS=: read -r name cpu memlim domain <<<"$spec"
  mkecho "$name"
  create_app "$name" --data cpu="$cpu" --data memory="$memlim" --data domain="$domain" >/dev/null
  check "deploy $name" deploy_is succeeded "$name" v1
  PORTS[$name]="$(port_of "$name")"
  if [ "$first" = 1 ]; then
    first=0
    # The fake Cloudflare issues a 10-day first certificate; the next sync
    # (every Add app syncs) renews it, so look at the warning now.
    check "certificate warning while the first (10-day) certificate is in use" wait_for 30 page_has / "expires in"
    refresh_csrf /settings
    post /settings/edge-sync >/dev/null
    check "renewed certificate clears the warning" page_lacks / "and has not been renewed"
  fi
done
wait_for 30 dns_is "$APP_HOST"
wait_for 60 all_healthy

say "Samples for the server, dootd and every app"
check "server sampled" wait_for 30 has_scope _host
for s in _dootd $APPS; do check "$s sampled" wait_for 30 has_scope "$s"; done
check "server memory total matches /proc/meminfo (within 5%)" between "$(python3 -c "print(abs($(m _host mem_limit)/($(awk '/^MemTotal/ {print $2}' /proc/meminfo)*1024)-1))")" 0 0.05
check "app memory limit reported (oom: 64 MB)" test "$(m oom mem_limit)" = 67108864

say "CPU: dootd's numbers against the kernel's counters"
local_app one "/burn?s=40" >/dev/null
local_app half "/burn?s=40" >/dev/null
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
local_app mem "/alloc?mb=60" >/dev/null
sleep 11
DM="$(m mem mem)"; KM="$(cat "$CG/mem/memory.current")"
echo "  mem: dootd $((DM >> 20)) MB, memory.current $((KM >> 20)) MB"
check "at least 59 MB reported" test "$DM" -ge $((59 << 20))
check "within 10% of memory.current" between "$(python3 -c "print(abs($DM-$KM)/$KM)")" 0 0.10

say "Requests, errors and latency"
for _ in $(seq 1 40); do app / >/dev/null; done
for _ in $(seq 1 10); do app /fail >/dev/null || true; done
# The sample holding the burst is the latest one for about 10 s: poll for it.
traffic_seen() { between "$(m one req)" 1 100000 && [ -n "$(m one p95)" ]; }
check "requests/min and p95 latency reported" wait_for 15 traffic_seen
check "5xx counted" test "$(fivexx)" -ge 10
check "apps without traffic report 0 requests" test "$(m half req)" = 0

say "Warnings"
local_app oom "/alloc?mb=100" >/dev/null 2>&1 || true
check "OOM kill shows a warning with the limit" wait_for 30 page_has / "oom was killed 1 time(s) in the last hour for using more than its 64 MB memory limit"
check "no disk warning at the default 85%" page_lacks / "Disk is"
test_env WARN_PERCENT=1
systemctl restart "$UNIT"
wait_for 60 all_healthy
check "disk warning at a 1% threshold" wait_for 30 page_has / "Disk is"
check "memory warning at a 1% threshold" page_has / "Memory is"
test_env
systemctl restart "$UNIT"
wait_for 60 all_healthy

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
check "rows older than 7 days pruned at startup" wait_for 30 no_old_rows
wait_for 60 all_healthy
check "24h chart still has data right after a restart" wait_for 20 svg_ok "scope=one&chart=cpu&range=24h" 1

say "Charts and pages"
sleep 11
check "server CPU chart (1h)" svg_ok "scope=_host&chart=cpu&range=1h" 1
check "server memory chart has used + swap lines" svg_ok "scope=_host&chart=memory&range=1h" 2
check "server load chart (7d)" svg_ok "scope=_host&chart=load&range=7d" 1
check "server disk chart (24h)" svg_ok "scope=_host&chart=disk&range=24h" 1
check "dootd memory chart" svg_ok "scope=_dootd&chart=memory&range=1h" 1
check "app requests chart has requests + 5xx" svg_ok "scope=one&chart=requests&range=24h" 2
check "app latency chart has p50 + p95" svg_ok "scope=one&chart=latency&range=24h" 2
check "unknown scope: 404" test "$(dc -o /dev/null -w '%{http_code}' "$DASH/charts?scope=nope&chart=cpu")" = 404
check "unknown chart: 404" test "$(dc -o /dev/null -w '%{http_code}' "$DASH/charts?scope=one&chart=nope")" = 404
check "charts need a session" test "$(cfcurl -o /dev/null -w '%{http_code}' "$DASH/charts?scope=_host&chart=cpu")" = 303
check "monitoring page lists every app" lists_apps
check "monitoring page embeds the charts" page_has /metrics '<img src="/charts?scope=_host&amp;chart=cpu&amp;range=1h'
check "app page has a usage section with 4 charts" four_charts
check "range links switch the charts" page_has "/apps/one?range=7d" "range=7d&amp;t="

say "dootd's own budget: idle with 5 apps (Req 1.3)"
systemctl restart "$UNIT"
wait_for 60 all_healthy
sleep 30
PID="$(systemctl show -p MainPID --value "$UNIT")"
T0="$(awk '{print $14+$15}' "/proc/$PID/stat")"
sleep 60
T1="$(awk '{print $14+$15}' "/proc/$PID/stat")"
RSS="$(awk '/^VmRSS/ {print $2*1024}' "/proc/$PID/status")"
CPU="$(python3 -c "print(($T1-$T0)/100/60*100)")"
echo "  dootd: $((RSS >> 20)) MB resident, ${CPU}% CPU over 60 s"
grep -E '^(Rss|Pss_Anon|Pss_File|Anonymous)' "/proc/$PID/smaps_rollup" | sed 's/^/    /' || true
check "resident memory under 30 MB" test "$RSS" -lt $((30 << 20))
check "CPU under 1% while idle" between "$CPU" 0 1
check "dootd's self-report matches /proc (within 20%)" between "$(python3 -c "print(abs($(m _dootd mem)-$RSS)/$RSS)")" 0 0.2

e2e_result
