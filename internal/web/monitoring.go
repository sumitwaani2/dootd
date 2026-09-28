package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/sumitwaani2/dootd/internal/hostinfo"
	"github.com/sumitwaani2/dootd/internal/metrics"
)

// Thresholds for dashboard warnings (Req 16.4).
type Thresholds struct {
	DiskPercent   float64 // default 85
	MemoryPercent float64 // default 90
	CertDays      int     // default 14
}

func (t Thresholds) withDefaults() Thresholds {
	if t.DiskPercent <= 0 {
		t.DiskPercent = 85
	}
	if t.MemoryPercent <= 0 {
		t.MemoryPercent = 90
	}
	if t.CertDays <= 0 {
		t.CertDays = 14
	}
	return t
}

// chartSpec says which lines a chart has.
type chartSpec struct {
	format func(float64) string
	minTop float64
	lines  func(pts []metrics.Point, cpuLimit float64) []Line
}

func col(pts []metrics.Point, f func(metrics.Point) float64) []float64 {
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = f(p)
	}
	return out
}

const (
	blue = "#2f5bea"
	red  = "#b42318"
	grey = "#9ca3af"
	teal = "#0f766e"
)

var hostCharts = map[string]chartSpec{
	"cpu": {fmtPct, 100, func(p []metrics.Point, _ float64) []Line {
		return []Line{{Name: "CPU", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.CPU })}}
	}},
	"memory": {fmtBytes, 1 << 20, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "used", Color: blue, Values: col(p, func(p metrics.Point) float64 { return float64(p.Mem) })},
			{Name: "swap", Color: red, Values: col(p, func(p metrics.Point) float64 { return float64(p.Swap) })},
			{Name: "total", Color: grey, Dashed: true, Values: col(p, func(p metrics.Point) float64 { return float64(p.MemLimit) })},
		}
	}},
	"load": {fmtNum, 1, func(p []metrics.Point, cores float64) []Line {
		return []Line{
			{Name: "load", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.Load })},
			{Name: "CPUs", Color: grey, Dashed: true, Values: col(p, func(metrics.Point) float64 { return cores })},
		}
	}},
	"disk": {fmtBytes, 1 << 30, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "used", Color: blue, Values: col(p, func(p metrics.Point) float64 { return float64(p.DiskUsed) })},
			{Name: "size", Color: grey, Dashed: true, Values: col(p, func(p metrics.Point) float64 { return float64(p.DiskTotal) })},
		}
	}},
}

var dootdCharts = map[string]chartSpec{
	"cpu": {fmtPct, 10, func(p []metrics.Point, _ float64) []Line {
		return []Line{{Name: "CPU (of one core)", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.CPU })}}
	}},
	"memory": {fmtBytes, 32 << 20, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "resident", Color: blue, Values: col(p, func(p metrics.Point) float64 { return float64(p.Mem) })},
			{Name: "target", Color: grey, Dashed: true, Values: col(p, func(metrics.Point) float64 { return 30 << 20 })},
		}
	}},
}

var appCharts = map[string]chartSpec{
	"cpu": {fmtPct, 10, func(p []metrics.Point, limit float64) []Line {
		return []Line{
			{Name: "CPU (of one core)", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.CPU })},
			{Name: "limit", Color: grey, Dashed: true, Values: col(p, func(metrics.Point) float64 { return limit })},
		}
	}},
	"memory": {fmtBytes, 1 << 20, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "used", Color: blue, Values: col(p, func(p metrics.Point) float64 { return float64(p.Mem) })},
			{Name: "limit", Color: grey, Dashed: true, Values: col(p, func(p metrics.Point) float64 { return float64(p.MemLimit) })},
		}
	}},
	"requests": {fmtNum, 10, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "requests/min", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.Req })},
			{Name: "5xx/min", Color: red, Values: col(p, func(p metrics.Point) float64 { return p.Req5xx })},
		}
	}},
	"latency": {fmtMs, 10, func(p []metrics.Point, _ float64) []Line {
		return []Line{
			{Name: "p50", Color: teal, Values: col(p, func(p metrics.Point) float64 { return p.P50 })},
			{Name: "p95", Color: blue, Values: col(p, func(p metrics.Point) float64 { return p.P95 })},
		}
	}},
}

// chartSVG serves /charts?scope=&chart=&range= as an SVG image.
func (s *Server) chartSVG(w http.ResponseWriter, r *http.Request) {
	scope, name := r.URL.Query().Get("scope"), r.URL.Query().Get("chart")
	rng := metrics.ParseRange(r.URL.Query().Get("range"))
	var specs map[string]chartSpec
	extra := 0.0
	switch scope {
	case metrics.Host:
		specs = hostCharts
		extra = float64(s.hostCPUs())
	case metrics.Dootd:
		specs = dootdCharts
	default:
		a := s.Sup.Get(scope)
		if a == nil {
			http.NotFound(w, r)
			return
		}
		specs = appCharts
		extra = a.Spec().Limits.CPUMax * 100
	}
	spec, ok := specs[name]
	if !ok || s.Metrics == nil {
		http.NotFound(w, r)
		return
	}
	pts, step, err := s.Metrics.Series(r.Context(), scope, rng)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	times := make([]time.Time, len(pts))
	for i, p := range pts {
		times[i] = p.TS
	}
	now := time.Now()
	c := Chart{Times: times, Lines: spec.lines(pts, extra), From: now.Add(-rng.Duration()), To: now,
		Gap: time.Duration(2.5 * float64(step)), Format: spec.format, MinTop: spec.minTop,
		Bytes: name == "memory" || name == "disk"}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(c.SVG())
}

func (s *Server) hostCPUs() int { return hostinfo.Read(s.Layout.Root).CPUs }

// ChartRef is a chart on a page.
type ChartRef struct {
	Title string
	URL   string
}

func chartRefs(scope string, rng metrics.Range, titles [][2]string) []ChartRef {
	stamp := time.Now().Unix() / 10 // new URL every sample so refreshes are not cached
	var out []ChartRef
	for _, t := range titles {
		out = append(out, ChartRef{Title: t[1], URL: fmt.Sprintf("/charts?scope=%s&chart=%s&range=%s&t=%d", scope, t[0], rng, stamp)})
	}
	return out
}

// metricsPage shows the host and dootd itself.
func (s *Server) metricsPage(w http.ResponseWriter, r *http.Request) {
	rng := metrics.ParseRange(r.URL.Query().Get("range"))
	data := map[string]any{
		"Range":  string(rng),
		"Ranges": []string{"1h", "24h", "7d"},
		"Host": chartRefs(metrics.Host, rng, [][2]string{
			{"cpu", "CPU (all cores)"}, {"memory", "Memory"}, {"load", "Load average"}, {"disk", "Disk (" + s.Layout.Root + ")"},
		}),
		"Dootd": chartRefs(metrics.Dootd, rng, [][2]string{{"cpu", "dootd CPU"}, {"memory", "dootd memory"}}),
	}
	if p, ok := s.Metrics.Latest(metrics.Dootd); ok {
		data["Self"] = p
	}
	if p, ok := s.Metrics.Latest(metrics.Host); ok {
		data["HostNow"] = p
	}
	rows, _ := s.appRows(r.Context())
	type appNow struct {
		AppRow
		Now metrics.Point
		OK  bool
	}
	var list []appNow
	for _, row := range rows {
		p, ok := s.Metrics.Latest(row.Name)
		list = append(list, appNow{row, p, ok})
	}
	data["Apps"] = list
	s.render(w, r, http.StatusOK, "metrics", "Monitoring", "metrics", data)
}

// metricWarnings adds host and app resource warnings (Req 16.4).
func (s *Server) metricWarnings(ctx context.Context, rows []AppRow) []string {
	if s.Metrics == nil {
		return nil
	}
	th := s.Thresholds.withDefaults()
	var ws []string
	if p, ok := s.Metrics.Latest(metrics.Host); ok {
		if p.DiskTotal > 0 {
			if pct := 100 * float64(p.DiskUsed) / float64(p.DiskTotal); pct > th.DiskPercent {
				ws = append(ws, fmt.Sprintf("Disk is %.0f%% full (%s free). Builds, logs and backups need space.", pct, fmtBytes(float64(p.DiskTotal-p.DiskUsed))))
			}
		}
		if p.MemLimit > 0 {
			if pct := 100 * float64(p.Mem) / float64(p.MemLimit); pct > th.MemoryPercent {
				ws = append(ws, fmt.Sprintf("Memory is %.0f%% used (%s of %s). Apps may be killed for running out of memory.", pct, fmtBytes(float64(p.Mem)), fmtBytes(float64(p.MemLimit))))
			}
		}
	}
	for _, r := range rows {
		if n := s.Metrics.OOMSince(r.Name, time.Hour); n > 0 {
			ws = append(ws, fmt.Sprintf("%s was killed %d time(s) in the last hour for using more than its %s memory limit. Raise the limit in its settings or reduce its memory use.",
				r.Name, n, fmtBytes(float64(r.Limits.MemoryMax))))
		} else if p, ok := s.Metrics.Latest(r.Name); ok && p.MemLimit > 0 && float64(p.Mem) > 0.9*float64(p.MemLimit) {
			ws = append(ws, fmt.Sprintf("%s is using %s, close to its %s memory limit.", r.Name, fmtBytes(float64(p.Mem)), fmtBytes(float64(p.MemLimit))))
		}
	}
	if s.Edge != nil {
		for _, h := range s.Edge.Status(ctx).Hosts {
			if !h.CertUntil.IsZero() {
				if d := time.Until(h.CertUntil); d < time.Duration(th.CertDays)*24*time.Hour {
					ws = append(ws, fmt.Sprintf("The certificate for %s expires in %d day(s) and has not been renewed; check the Cloudflare token (Settings).", h.Host, int(math.Max(0, d.Hours()/24))))
				}
			}
		}
	}
	return ws
}
