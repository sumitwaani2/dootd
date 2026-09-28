// Package metrics samples host, dootd and per-app resource usage and
// request statistics every 10 seconds, keeps the last hour in memory and
// stores 1-minute rollups for 7 days (docs/architecture.md §13).
package metrics

import (
	"bufio"
	"context"
	"database/sql"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/edge"
	"github.com/sumitwaani2/dootd/internal/hostinfo"
	"github.com/sumitwaani2/dootd/internal/store"
	"github.com/sumitwaani2/dootd/internal/supervisor"
)

// Scopes other than app names.
const (
	Host  = "_host"
	Dootd = "_dootd"
)

// Timings.
const (
	Every     = 10 * time.Second
	RingSpan  = time.Hour
	Keep      = 7 * 24 * time.Hour
	ringSize  = int(RingSpan / Every)
	nBuckets  = 12
	maxPoints = 360
)

// Point is one sample (10 s) or one rollup (1 min). Latency fields are
// NaN when there were no requests.
type Point struct {
	TS        time.Time `json:"ts"`
	CPU       float64   `json:"cpu"`       // percent (host: of all cores; apps, dootd: of one core)
	Mem       int64     `json:"mem"`       // bytes
	MemLimit  int64     `json:"mem_limit"` // host: total RAM; apps: memory.max
	Swap      int64     `json:"swap"`
	Load      float64   `json:"load1"`
	DiskUsed  int64     `json:"disk_used"`
	DiskTotal int64     `json:"disk_total"`
	Pids      int64     `json:"pids"`
	IORead    float64   `json:"io_read"`  // bytes/s
	IOWrite   float64   `json:"io_write"` // bytes/s
	Req       float64   `json:"req"`      // requests/min
	Req5xx    float64   `json:"req_5xx"`  // 5xx responses/min
	P50       float64   `json:"-"`        // ms
	P95       float64   `json:"-"`
	OOM       int64     `json:"oom"` // OOM kills in the interval
}

// Collector samples and stores metrics.
type Collector struct {
	Store    *store.Store
	Sup      *supervisor.Supervisor
	Requests func() map[string]edge.StatsSnapshot // nil without the edge
	DataRoot string
	Log      *slog.Logger

	mu    sync.RWMutex
	rings map[string][]Point // newest last, at most ringSize
	prev  map[string]counters
	host  cpuTimes
	self  int64 // dootd CPU ticks
	last  time.Time
	cur   map[string][]rawSample // samples of the current minute
	curM  time.Time
	done  chan struct{}
}

type counters struct {
	cpuUsec, ioR, ioW, oom int64
	req, r5xx              int64
	buckets                [nBuckets]int64
	ok                     bool
}

type rawSample struct {
	p       Point
	secs    float64
	req     int64
	r5xx    int64
	buckets [nBuckets]int64
}

type cpuTimes struct{ total, idle uint64 }

// Run samples every 10 s, writes rollups every minute and prunes hourly.
func (c *Collector) Run(ctx context.Context) {
	c.mu.Lock()
	c.rings, c.prev, c.cur = map[string][]Point{}, map[string]counters{}, map[string][]rawSample{}
	c.done = make(chan struct{})
	c.mu.Unlock()
	go func() {
		defer close(c.done)
		c.prune(ctx)
		c.sample(time.Now())
		t := time.NewTicker(Every)
		defer t.Stop()
		lastPrune := time.Now()
		for {
			select {
			case <-ctx.Done():
				c.flush(context.Background())
				return
			case now := <-t.C:
				c.sample(now)
				if time.Since(lastPrune) > time.Hour {
					c.prune(ctx)
					lastPrune = time.Now()
				}
			}
		}
	}()
}

func (c *Collector) sample(now time.Time) {
	c.mu.Lock()
	secs := now.Sub(c.last).Seconds()
	first := c.last.IsZero()
	c.last = now
	c.mu.Unlock()
	if first || secs <= 0 {
		secs = Every.Seconds()
	}
	var reqs map[string]edge.StatsSnapshot
	if c.Requests != nil {
		reqs = c.Requests()
	}

	samples := map[string]rawSample{}
	// Host.
	hi := hostinfo.Read(c.DataRoot)
	ht := readCPUTimes()
	hp := Point{TS: now, Mem: hi.MemTotal - hi.MemAvailable, MemLimit: hi.MemTotal, Swap: hi.SwapTotal - hi.SwapFree,
		Load: hi.Load1, DiskUsed: hi.DiskTotal - hi.DiskFree, DiskTotal: hi.DiskTotal, P50: math.NaN(), P95: math.NaN()}
	c.mu.Lock()
	if c.host.total > 0 && ht.total > c.host.total {
		dt, di := float64(ht.total-c.host.total), float64(ht.idle-c.host.idle)
		hp.CPU = clamp(100*(dt-di)/dt, 0, 100)
	}
	c.host = ht
	// dootd itself.
	rss, ticks := readSelf()
	sp := Point{TS: now, Mem: rss, Pids: int64(threads()), P50: math.NaN(), P95: math.NaN()}
	if c.self > 0 && ticks >= c.self {
		sp.CPU = 100 * float64(ticks-c.self) / clockTicks / secs
	}
	c.self = ticks
	c.mu.Unlock()
	samples[Host] = rawSample{p: hp, secs: secs}
	samples[Dootd] = rawSample{p: sp, secs: secs}

	// Apps.
	for _, a := range c.Sup.Apps() {
		name := a.Spec().Name
		gs, err := a.Group().Stats()
		if err != nil {
			continue
		}
		rs := reqs[name]
		cur := counters{cpuUsec: gs.CPUUsageUsec, ioR: gs.IOReadBytes, ioW: gs.IOWriteBytes, oom: gs.OOMKills,
			req: rs.Requests, r5xx: rs.Status[5], buckets: rs.Buckets, ok: true}
		c.mu.Lock()
		prev := c.prev[name]
		c.prev[name] = cur
		c.mu.Unlock()
		p := Point{TS: now, Mem: gs.MemoryCurrent, MemLimit: a.Spec().Limits.MemoryMax, Pids: gs.PidsCurrent, P50: math.NaN(), P95: math.NaN()}
		r := rawSample{p: p, secs: secs}
		if prev.ok {
			p.CPU = 100 * float64(delta(cur.cpuUsec, prev.cpuUsec)) / 1e6 / secs
			p.IORead = float64(delta(cur.ioR, prev.ioR)) / secs
			p.IOWrite = float64(delta(cur.ioW, prev.ioW)) / secs
			p.OOM = delta(cur.oom, prev.oom)
			r.req, r.r5xx = delta(cur.req, prev.req), delta(cur.r5xx, prev.r5xx)
			for i := range r.buckets {
				r.buckets[i] = delta(cur.buckets[i], prev.buckets[i])
			}
			p.Req, p.Req5xx = float64(r.req)*60/secs, float64(r.r5xx)*60/secs
			p.P50, p.P95 = percentile(r.buckets, 0.50), percentile(r.buckets, 0.95)
		}
		r.p = p
		samples[name] = r
	}

	minute := now.Truncate(time.Minute)
	c.mu.Lock()
	var flush map[string][]rawSample
	var flushM time.Time
	if !c.curM.IsZero() && minute.After(c.curM) {
		flush, flushM = c.cur, c.curM
		c.cur = map[string][]rawSample{}
	}
	c.curM = minute
	for scope, s := range samples {
		ring := append(c.rings[scope], s.p)
		if len(ring) > ringSize {
			ring = ring[len(ring)-ringSize:]
		}
		c.rings[scope] = ring
		c.cur[scope] = append(c.cur[scope], s)
	}
	// Forget apps that no longer exist.
	for scope := range c.rings {
		if _, ok := samples[scope]; !ok {
			delete(c.rings, scope)
			delete(c.prev, scope)
		}
	}
	c.mu.Unlock()
	if flush != nil {
		c.write(context.Background(), flushM, flush)
	}
}

// Wait blocks until Run has written the last partial minute after its
// context ended (so a restart loses no data).
func (c *Collector) Wait() {
	c.mu.RLock()
	done := c.done
	c.mu.RUnlock()
	if done != nil {
		<-done
	}
}

// flush writes the current partial minute (at shutdown).
func (c *Collector) flush(ctx context.Context) {
	c.mu.Lock()
	cur, m := c.cur, c.curM
	c.cur = map[string][]rawSample{}
	c.mu.Unlock()
	if len(cur) > 0 {
		c.write(ctx, m, cur)
	}
}

func (c *Collector) write(ctx context.Context, minute time.Time, bySc map[string][]rawSample) {
	tx, err := c.Store.Writer().BeginTx(ctx, nil)
	if err != nil {
		c.Log.Warn("metrics rollup", "err", err)
		return
	}
	defer tx.Rollback()
	for scope, ss := range bySc {
		p := rollup(ss)
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO metrics_1m (scope, ts, cpu, mem, mem_limit, swap, load1, disk_used,
			disk_total, pids, io_read, io_write, req, req_5xx, p50, p95, oom) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			scope, minute.Unix(), p.CPU, p.Mem, p.MemLimit, p.Swap, p.Load, p.DiskUsed, p.DiskTotal, p.Pids,
			p.IORead, p.IOWrite, p.Req, p.Req5xx, nullable(p.P50), nullable(p.P95), p.OOM); err != nil {
			c.Log.Warn("metrics rollup", "err", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		c.Log.Warn("metrics rollup", "err", err)
	}
}

// rollup combines one minute of samples: averages for rates and CPU,
// maxima for memory and processes, sums for OOM kills, and latency
// percentiles from the summed histograms.
func rollup(ss []rawSample) Point {
	var p Point
	var secs float64
	var req, r5xx int64
	var buckets [nBuckets]int64
	for _, s := range ss {
		secs += s.secs
		p.CPU += s.p.CPU * s.secs
		p.IORead += s.p.IORead * s.secs
		p.IOWrite += s.p.IOWrite * s.secs
		p.Load += s.p.Load * s.secs
		p.Mem = max(p.Mem, s.p.Mem)
		p.Swap = max(p.Swap, s.p.Swap)
		p.Pids = max(p.Pids, s.p.Pids)
		p.MemLimit, p.DiskUsed, p.DiskTotal = s.p.MemLimit, s.p.DiskUsed, s.p.DiskTotal
		p.OOM += s.p.OOM
		req += s.req
		r5xx += s.r5xx
		for i := range buckets {
			buckets[i] += s.buckets[i]
		}
	}
	if secs > 0 {
		p.CPU, p.IORead, p.IOWrite, p.Load = p.CPU/secs, p.IORead/secs, p.IOWrite/secs, p.Load/secs
		p.Req, p.Req5xx = float64(req)*60/secs, float64(r5xx)*60/secs
	}
	p.P50, p.P95 = percentile(buckets, 0.50), percentile(buckets, 0.95)
	return p
}

func (c *Collector) prune(ctx context.Context) {
	if _, err := c.Store.Writer().ExecContext(ctx, `DELETE FROM metrics_1m WHERE ts < ?`, time.Now().Add(-Keep).Unix()); err != nil {
		c.Log.Warn("metrics prune", "err", err)
	}
}

// Latest returns the newest sample of scope.
func (c *Collector) Latest(scope string) (Point, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r := c.rings[scope]
	if len(r) == 0 {
		return Point{}, false
	}
	return r[len(r)-1], true
}

// Scopes lists scopes with samples (host, dootd, then apps sorted).
func (c *Collector) Scopes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var apps []string
	for s := range c.rings {
		if s != Host && s != Dootd {
			apps = append(apps, s)
		}
	}
	sort.Strings(apps)
	return append([]string{Host, Dootd}, apps...)
}

// Range is a chart time range.
type Range string

// Ranges.
const (
	Hour Range = "1h"
	Day  Range = "24h"
	Week Range = "7d"
)

// Duration of the range.
func (r Range) Duration() time.Duration {
	switch r {
	case Day:
		return 24 * time.Hour
	case Week:
		return Keep
	}
	return time.Hour
}

// ParseRange accepts 1h, 24h and 7d (default 1h).
func ParseRange(s string) Range {
	switch Range(s) {
	case Day, Week:
		return Range(s)
	}
	return Hour
}

// Series returns up to 360 points of scope covering the range, from the
// in-memory samples and the stored rollups, and the bucket width used
// (points further apart than 2.5 widths are gaps).
func (c *Collector) Series(ctx context.Context, scope string, r Range) ([]Point, time.Duration, error) {
	now := time.Now()
	from := now.Add(-r.Duration())
	c.mu.RLock()
	ring := append([]Point(nil), c.rings[scope]...)
	c.mu.RUnlock()
	until := now
	if r == Hour && len(ring) > 0 {
		until = ring[0].TS // rollups only before the first sample in memory
	}
	rows, err := c.rows(ctx, scope, from, until)
	if err != nil {
		return nil, 0, err
	}
	pts := rows
	lastRow := from
	if len(rows) > 0 {
		lastRow = rows[len(rows)-1].TS.Add(time.Minute)
	}
	for _, p := range ring {
		if !p.TS.Before(lastRow) && !p.TS.Before(from) {
			pts = append(pts, p)
		}
	}
	step := Every
	if r != Hour {
		step = time.Minute
	}
	width := r.Duration() / maxPoints
	if width <= step {
		return pts, step, nil
	}
	return downsample(pts, from, width), width, nil
}

func (c *Collector) rows(ctx context.Context, scope string, from, until time.Time) ([]Point, error) {
	rs, err := c.Store.Reader().QueryContext(ctx, `SELECT ts, cpu, mem, mem_limit, swap, load1, disk_used, disk_total, pids,
		io_read, io_write, req, req_5xx, p50, p95, oom FROM metrics_1m WHERE scope = ? AND ts >= ? AND ts < ? ORDER BY ts`,
		scope, from.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Point
	for rs.Next() {
		var p Point
		var ts int64
		var p50, p95 sql.NullFloat64
		if err := rs.Scan(&ts, &p.CPU, &p.Mem, &p.MemLimit, &p.Swap, &p.Load, &p.DiskUsed, &p.DiskTotal, &p.Pids,
			&p.IORead, &p.IOWrite, &p.Req, &p.Req5xx, &p50, &p95, &p.OOM); err != nil {
			return nil, err
		}
		p.TS = time.Unix(ts, 0)
		p.P50, p.P95 = nanIfNull(p50), nanIfNull(p95)
		out = append(out, p)
	}
	return out, rs.Err()
}

// downsample averages points into fixed-width buckets (maxima for memory,
// processes and p95; sums for OOM kills).
func downsample(pts []Point, from time.Time, width time.Duration) []Point {
	var out []Point
	var acc []Point
	var bucket int64 = -1
	emit := func() {
		if len(acc) == 0 {
			return
		}
		p := Point{P50: math.NaN()}
		n := float64(len(acc))
		p95, p50sum, p50n := math.NaN(), 0.0, 0.0
		for _, a := range acc {
			p.CPU += a.CPU / n
			p.Load += a.Load / n
			p.IORead += a.IORead / n
			p.IOWrite += a.IOWrite / n
			p.Req += a.Req / n
			p.Req5xx += a.Req5xx / n
			p.Mem = max(p.Mem, a.Mem)
			p.Swap = max(p.Swap, a.Swap)
			p.Pids = max(p.Pids, a.Pids)
			p.OOM += a.OOM
			p.MemLimit, p.DiskUsed, p.DiskTotal = a.MemLimit, a.DiskUsed, a.DiskTotal
			if !math.IsNaN(a.P50) {
				p50sum += a.P50
				p50n++
			}
			if !math.IsNaN(a.P95) && (math.IsNaN(p95) || a.P95 > p95) {
				p95 = a.P95
			}
		}
		if p50n > 0 {
			p.P50 = p50sum / p50n
		}
		p.P95 = p95
		p.TS = from.Add(time.Duration(bucket)*width + width/2)
		out = append(out, p)
		acc = acc[:0]
	}
	for _, p := range pts {
		b := int64(p.TS.Sub(from) / width)
		if b != bucket {
			emit()
			bucket = b
		}
		acc = append(acc, p)
	}
	emit()
	return out
}

// OOMSince sums OOM kills of scope in the last d (at most 1 hour of samples).
func (c *Collector) OOMSince(scope string, d time.Duration) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var n int64
	cut := time.Now().Add(-d)
	for _, p := range c.rings[scope] {
		if p.TS.After(cut) {
			n += p.OOM
		}
	}
	return n
}

// percentile estimates a latency percentile from histogram counts using
// bucket upper bounds (the overflow bucket reports 2x the last bound).
func percentile(b [nBuckets]int64, q float64) float64 {
	var total int64
	for _, n := range b {
		total += n
	}
	if total == 0 {
		return math.NaN()
	}
	want := int64(math.Ceil(q * float64(total)))
	var cum int64
	for i, n := range b {
		cum += n
		if cum >= want {
			if i < len(edge.LatencyBuckets) {
				return edge.LatencyBuckets[i]
			}
			return 2 * edge.LatencyBuckets[len(edge.LatencyBuckets)-1]
		}
	}
	return math.NaN()
}

func delta(cur, prev int64) int64 {
	if cur < prev { // counter reset (app re-created, route changed)
		return cur
	}
	return cur - prev
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func nullable(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

func nanIfNull(v sql.NullFloat64) float64 {
	if !v.Valid {
		return math.NaN()
	}
	return v.Float64
}

func readCPUTimes() cpuTimes {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 5 || fs[0] != "cpu" {
		return cpuTimes{}
	}
	var t cpuTimes
	for i, v := range fs[1:] {
		if i >= 8 { // guest time is already included in user/nice
			break
		}
		n, _ := strconv.ParseUint(v, 10, 64)
		t.total += n
		if i == 3 || i == 4 { // idle, iowait
			t.idle += n
		}
	}
	return t
}

// clockTicks is USER_HZ, 100 on every Linux platform dootd supports.
const clockTicks = 100

// readSelf returns dootd's resident memory and CPU ticks (user+system).
func readSelf() (rss int64, ticks int64) {
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
				n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
				rss = n << 10
			}
		}
	}
	if b, err := os.ReadFile("/proc/self/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i > 0 {
			fs := strings.Fields(s[i+1:])
			if len(fs) > 12 { // fields 14 and 15 of stat (utime, stime)
				u, _ := strconv.ParseInt(fs[11], 10, 64)
				st, _ := strconv.ParseInt(fs[12], 10, 64)
				ticks = u + st
			}
		}
	}
	return rss, ticks
}

func threads() int {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0
	}
	return len(entries)
}
