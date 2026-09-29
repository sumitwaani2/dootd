// Package edge is dootd's public front door: a :443 listener that only
// accepts Cloudflare, TLS with Cloudflare Origin CA certificates and
// Authenticated Origin Pulls, and a Host-header router that proxies to
// apps on 127.0.0.1 (docs/architecture.md §9).
package edge

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// builtinRanges is the fallback list of Cloudflare edge ranges, used until
// the first successful refresh (https://api.cloudflare.com/client/v4/ips).
var builtinRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// IPFilter holds the allowed source ranges.
type IPFilter struct {
	prefixes atomic.Pointer[[]netip.Prefix]
	rejected atomic.Int64
	source   atomic.Value // string: "built-in", "saved", "cloudflare"
}

// NewIPFilter starts with the built-in Cloudflare ranges.
func NewIPFilter() *IPFilter {
	f := &IPFilter{}
	if err := f.Set(builtinRanges, "built-in"); err != nil {
		panic(err)
	}
	return f
}

// Set replaces the allowed ranges. It refuses empty or invalid lists so a
// bad refresh can never lock Cloudflare out or open the door to everyone.
func (f *IPFilter) Set(cidrs []string, source string) error {
	var ps []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return fmt.Errorf("edge: bad CIDR %q: %w", c, err)
		}
		if p.Bits() < 8 && p.Addr().Is4() || p.Bits() < 16 && p.Addr().Is6() {
			return fmt.Errorf("edge: CIDR %q is implausibly large for Cloudflare", c)
		}
		ps = append(ps, p.Masked())
	}
	if len(ps) == 0 {
		return fmt.Errorf("edge: empty IP range list")
	}
	f.prefixes.Store(&ps)
	f.source.Store(source)
	return nil
}

// Ranges returns the current ranges (sorted) and where they came from.
func (f *IPFilter) Ranges() ([]string, string) {
	ps := *f.prefixes.Load()
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	sort.Strings(out)
	src, _ := f.source.Load().(string)
	return out, src
}

// Allowed reports whether addr may connect.
func (f *IPFilter) Allowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range *f.prefixes.Load() {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Rejected is the number of connections closed by the filter.
func (f *IPFilter) Rejected() int64 { return f.rejected.Load() }

// filteredListener closes non-Cloudflare connections before any TLS work,
// except while setupOpen reports true: then they are let through as
// setup connections (docs/architecture.md §8.1).
type filteredListener struct {
	net.Listener
	f         *IPFilter
	log       *slog.Logger
	setupOpen func() bool

	mu       sync.Mutex
	lastLog  time.Time
	sinceLog int64
}

// Listen wraps ln with the filter. setupOpen may be nil (never open).
func (f *IPFilter) Listen(ln net.Listener, log *slog.Logger, setupOpen func() bool) net.Listener {
	return &filteredListener{Listener: ln, f: f, log: log, setupOpen: setupOpen}
}

func (l *filteredListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err == nil && l.f.Allowed(ap.Addr()) {
			return c, nil
		}
		if err == nil && l.setupOpen != nil && l.setupOpen() {
			return &setupConn{Conn: c}, nil
		}
		c.Close()
		l.f.rejected.Add(1)
		l.noteRejected(c.RemoteAddr().String())
	}
}

// noteRejected logs at most one line per minute.
func (l *filteredListener) noteRejected(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sinceLog++
	if time.Since(l.lastLog) < time.Minute {
		return
	}
	l.log.Warn("rejected connections from outside Cloudflare", "count", l.sinceLog, "latest", addr)
	l.lastLog, l.sinceLog = time.Now(), 0
}
