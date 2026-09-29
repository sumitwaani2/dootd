package edge

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Route maps a hostname to an app.
type Route struct {
	Host string
	App  string
	Port int
}

// Availability is an app's readiness for traffic.
type Availability int

// Availability values.
const (
	Available  Availability = iota
	Deploying               // 503 "deploying"
	Starting                // 503 "starting"
	NotRunning              // 503 "not running"
)

// Router routes by Host header to the dashboard or to app proxies.
// Requests on setup connections only ever reach the dashboard.
type Router struct {
	Dashboard http.Handler
	// State tells the router whether app can take traffic.
	State func(app string) Availability
	// SetupOpen reports whether the setup address is open.
	SetupOpen func() bool
	Log       *slog.Logger

	dashHost atomic.Value // string
	mu       sync.RWMutex
	routes   map[string]*appProxy
}

// SetDashboardHost changes the dashboard domain ("" = none).
func (rt *Router) SetDashboardHost(h string) { rt.dashHost.Store(h) }

// DashboardHost is the dashboard domain ("" = none).
func (rt *Router) DashboardHost() string {
	h, _ := rt.dashHost.Load().(string)
	return h
}

type appProxy struct {
	route Route
	proxy *httputil.ReverseProxy
	stats *Stats
}

// SetRoutes replaces the routing table. Stats survive for unchanged apps.
func (rt *Router) SetRoutes(routes []Route) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	old := rt.routes
	rt.routes = map[string]*appProxy{}
	for _, r := range routes {
		if p, ok := old[r.Host]; ok && p.route == r {
			rt.routes[r.Host] = p
			continue
		}
		rt.routes[r.Host] = rt.newProxy(r)
	}
}

// Stats returns per-app request statistics.
func (rt *Router) Stats() map[string]StatsSnapshot {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := map[string]StatsSnapshot{}
	for _, p := range rt.routes {
		out[p.route.App] = p.stats.Snapshot()
	}
	return out
}

func (rt *Router) newProxy(r Route) *appProxy {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port))}
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		// No response timeout: long-polling and streaming must work.
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // apps see their own domain
			ip := ClientIP(pr.In)
			pr.Out.Header.Set("X-Forwarded-For", ip)
			pr.Out.Header.Set("X-Real-IP", ip)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
		},
		Transport:     tr,
		FlushInterval: -1, // stream responses (SSE, long-poll) immediately
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // client went away
			}
			rt.Log.Warn("proxy error", "app", r.App, "err", err)
			page(w, http.StatusBadGateway, "App unreachable", "The app did not answer. It may have just crashed; dootd restarts it automatically.", 5)
		},
	}
	return &appProxy{route: r, proxy: p, stats: &Stats{}}
}

// ClientIP is the visitor's IP: CF-Connecting-IP (only Cloudflare can
// connect, so it is trustworthy), else the TCP peer. On the setup address
// the header could be forged, so only the TCP peer counts there.
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" && !IsDirect(r) {
		if a, err := netip.ParseAddr(v); err == nil {
			return a.String()
		}
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return r.RemoteAddr
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if IsDirect(r) {
		if rt.SetupOpen == nil || !rt.SetupOpen() {
			msg := "Setup is finished. Open the dashboard at its domain."
			if h := rt.DashboardHost(); h != "" {
				msg = "Setup is finished. The dashboard is at https://" + h + "/"
			}
			w.Header().Set("Connection", "close")
			page(w, http.StatusForbidden, "Use the dashboard domain", msg, 0)
			return
		}
		rt.Dashboard.ServeHTTP(w, r)
		return
	}
	host := normalizeHost(r.Host)
	// Host must match the TLS SNI, so a request can't use one zone's TLS
	// settings (e.g. without AOP) to reach another zone's app.
	if r.TLS != nil && normalizeHost(r.TLS.ServerName) != host {
		page(w, http.StatusMisdirectedRequest, "Misdirected request", "The Host header does not match the TLS server name.", 0)
		return
	}
	if dh := rt.DashboardHost(); dh != "" && host == dh {
		rt.Dashboard.ServeHTTP(w, r)
		return
	}
	rt.mu.RLock()
	p := rt.routes[host]
	rt.mu.RUnlock()
	if p == nil {
		page(w, http.StatusNotFound, "Not found", "No app is configured for this domain.", 0)
		return
	}

	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	switch rt.State(p.route.App) {
	case Deploying:
		page(sw, http.StatusServiceUnavailable, "Deploying", "A new version is being deployed. This takes a few seconds.", 3)
	case Starting:
		page(sw, http.StatusServiceUnavailable, "Starting", "The app is starting. Please retry in a moment.", 3)
	case NotRunning:
		page(sw, http.StatusServiceUnavailable, "App not running", "The app is stopped or has crashed.", 30)
	default:
		p.proxy.ServeHTTP(sw, r)
	}
	p.stats.observe(sw.code(), time.Since(start))
}

// page writes a small self-contained HTML error page.
func page(w http.ResponseWriter, code int, title, msg string, retryAfter int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Dootd-Page", strconv.Itoa(code))
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>%[1]d %[2]s</title><style>body{font:16px system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#333}`+
		`h1{font-size:1.4rem}</style><h1>%[2]s</h1><p>%[3]s</p><p><small>%[1]d · dootd</small></p>`+"\n",
		code, html.EscapeString(title), html.EscapeString(msg))
}

// statusWriter records the status code. Unwrap lets the reverse proxy
// reach Hijack/Flush on the underlying writer (WebSockets, streaming).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// LatencyBuckets are the histogram bucket upper bounds in milliseconds;
// the last bucket (index len) collects everything slower.
var LatencyBuckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

var latencyBuckets = LatencyBuckets

// Stats are cumulative request counters for one app (Phase 6 samples them).
type Stats struct {
	total   atomic.Int64
	classes [6]atomic.Int64 // index = status/100
	buckets [12]atomic.Int64
}

func (s *Stats) observe(code int, d time.Duration) {
	s.total.Add(1)
	if c := code / 100; c >= 1 && c <= 5 {
		s.classes[c].Add(1)
	}
	ms := float64(d.Microseconds()) / 1000
	i := 0
	for i < len(latencyBuckets) && ms > latencyBuckets[i] {
		i++
	}
	s.buckets[i].Add(1)
}

// StatsSnapshot is a copy of the counters.
type StatsSnapshot struct {
	Requests int64     `json:"requests"`
	Status   [6]int64  `json:"status_classes"` // [_,1xx,2xx,3xx,4xx,5xx]
	Buckets  [12]int64 `json:"latency_buckets"`
}

// Snapshot copies the counters.
func (s *Stats) Snapshot() StatsSnapshot {
	var out StatsSnapshot
	out.Requests = s.total.Load()
	for i := range s.classes {
		out.Status[i] = s.classes[i].Load()
	}
	for i := range s.buckets {
		out.Buckets[i] = s.buckets[i].Load()
	}
	return out
}
