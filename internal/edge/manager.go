package edge

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/cloudflare"
	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
)

// Settings keys.
const (
	SettingCFToken         = "cloudflare_token"
	purposeCFToken         = "settings:cloudflare_token"
	settingCFRanges        = "cloudflare_ip_ranges"
	SettingDashboardDomain = "dashboard_domain"
	// The dashboard domain that was last reached through Cloudflare with AOP.
	settingDashboardReached = "dashboard_reached"
)

// Timings.
const (
	certRenewBefore = 30 * 24 * time.Hour
	certDays        = 5475 // 15 years, the Origin CA maximum
	syncEvery       = 6 * time.Hour
	ipRefreshEvery  = 24 * time.Hour
	aopActivateWait = 3 * time.Minute
	// AOPRollout is how long dootd waits, after Cloudflare reports a newly
	// uploaded client certificate active (or zone-level AOP newly turned
	// on), before it requires the certificate. Cloudflare's edge servers
	// pick the change up gradually: on a real VPS some of them kept
	// connecting without it for almost 5 minutes, and every such request
	// failed with error 520 (D43).
	AOPRollout = 10 * time.Minute
)

// Config configures the edge. Nothing here is user-configurable: the
// public IPs and the API base are only overridden by the E2E tests.
type Config struct {
	Listen     string // ":443"
	PublicIPv4 string // "" = auto-detect
	PublicIPv6 string // "" = auto-detect, "off" = no AAAA records
	AOP        bool   // zone-level Authenticated Origin Pulls (always on in dootd serve)
	APIBase    string // Cloudflare API base (tests)
	DataRoot   string
	// AOPRollout overrides the default AOPRollout (tests; 0 = the default,
	// negative = enforce at once).
	AOPRollout time.Duration
	// SetupPending reports whether a one-time password is pending (§8.1).
	SetupPending func() bool
}

// Manager owns certificates, Cloudflare state and the listener.
type Manager struct {
	cfg    Config
	store  *store.Store
	box    *secrets.Box
	log    *slog.Logger
	Filter *IPFilter
	Certs  *CertStore
	Router *Router

	syncMu    sync.Mutex // one reconcile at a time
	setupCert *tls.Certificate

	mu        sync.RWMutex
	aop       *AOP
	dashHost  string
	dashSeen  string // dashboard domain reached through Cloudflare with AOP
	routes    []Route
	hosts     []hostEntry
	hostZone  map[string]string      // host -> zone id
	enforce   map[string]bool        // zone id -> require AOP client certs
	rollout   map[string]time.Time   // zone id -> AOP ready at Cloudflare since (not enforced yet)
	rollTimer map[string]*time.Timer // zone id -> sync when the rollout ends
	hostState map[string]*HostStatus
	zoneState map[string]*ZoneStatus
	lastSync  time.Time
	lastErr   string
	lastIPs   time.Time
	pubV4     string
	pubV6     string
}

type hostEntry struct {
	Host string
	App  string // "" for the dashboard
}

// HostStatus is the edge state of one hostname.
type HostStatus struct {
	Host      string    `json:"host"`
	App       string    `json:"app"`
	Zone      string    `json:"zone"`
	DNS       string    `json:"dns"`
	CertUntil time.Time `json:"cert_not_after"`
	Error     string    `json:"error,omitempty"`
}

// ZoneStatus is the state of one Cloudflare zone.
type ZoneStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SSLMode   string `json:"ssl_mode"` // "" = not known (never read)
	Status    string `json:"status"`   // Cloudflare zone status ("" = not known yet)
	AOP       string `json:"aop"`      // off, pending, enforced
	Warning   string `json:"warning,omitempty"`
	Error     string `json:"error,omitempty"`
	AOPCertID string `json:"-"`
	AOPSerial string `json:"-"`
}

// Status is a snapshot for the settings page.
type Status struct {
	Listen      string       `json:"listen"`
	Dashboard   string       `json:"dashboard_host"`
	TokenSet    bool         `json:"cloudflare_token_set"`
	PublicIPv4  string       `json:"public_ipv4"`
	PublicIPv6  string       `json:"public_ipv6"`
	IPRanges    int          `json:"ip_ranges"`
	IPSource    string       `json:"ip_ranges_source"`
	Rejected    int64        `json:"rejected_connections"`
	LastSync    time.Time    `json:"last_sync"`
	LastError   string       `json:"last_error,omitempty"`
	Hosts       []HostStatus `json:"hosts"`
	Zones       []ZoneStatus `json:"zones"`
	AOPClientTo time.Time    `json:"aop_client_not_after"`
}

// NewManager loads certificates, the AOP CA and saved Cloudflare state so
// TLS and AOP enforcement work immediately, even if the API is down.
func NewManager(ctx context.Context, cfg Config, st *store.Store, box *secrets.Box, log *slog.Logger) (*Manager, error) {
	m := &Manager{
		cfg: cfg, store: st, box: box, log: log.With("component", "edge"),
		Filter:    NewIPFilter(),
		hostZone:  map[string]string{},
		enforce:   map[string]bool{},
		rollout:   map[string]time.Time{},
		rollTimer: map[string]*time.Timer{},
		hostState: map[string]*HostStatus{},
		zoneState: map[string]*ZoneStatus{},
	}
	var err error
	if m.Certs, err = OpenCertStore(filepath.Join(cfg.DataRoot, "certs")); err != nil {
		return nil, err
	}
	if m.aop, _, err = LoadOrCreateAOP(filepath.Join(cfg.DataRoot, "aop")); err != nil {
		return nil, err
	}
	if v, ok, _ := st.GetSetting(ctx, settingCFRanges); ok {
		var rs []string
		if json.Unmarshal(v, &rs) == nil && m.Filter.Set(rs, "saved") == nil {
			m.log.Info("using saved Cloudflare IP ranges", "count", len(rs))
		}
	}
	if m.setupCert, err = loadOrCreateSetupCert(filepath.Join(cfg.DataRoot, "setup")); err != nil {
		return nil, err
	}
	if v, ok, _ := st.GetSetting(ctx, SettingDashboardDomain); ok {
		m.dashHost = string(v)
	}
	if v, ok, _ := st.GetSetting(ctx, settingDashboardReached); ok {
		m.dashSeen = string(v)
	}
	if err := m.loadState(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// DashboardHost is the dashboard domain ("" = not set yet).
func (m *Manager) DashboardHost() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dashHost
}

// DashboardReady reports whether the dashboard domain works through
// Cloudflare: its certificate is installed, AOP is enforced for its zone,
// the zone is active and its SSL/TLS mode makes Cloudflare connect to :443
// (Full or Full (strict)). Until then the setup address stays open, so a
// zone in Flexible mode (Cloudflare connects to :80, error 521) cannot lock
// the user out of the dashboard.
func (m *Manager) DashboardReady() bool {
	return m.DashboardProblem() == ""
}

// DashboardProblem says what the dashboard domain is still waiting for,
// in words for the dashboard; "" when it is ready.
func (m *Manager) DashboardProblem() string {
	m.mu.RLock()
	h := m.dashHost
	seen := h != "" && m.dashSeen == h
	zid := m.hostZone[h]
	// While Cloudflare rolls the AOP certificate out (D43) it is in place
	// at Cloudflare, just not required yet: that counts.
	_, rolling := m.rollout[zid]
	enforced := m.enforce[zid] || rolling
	var zs ZoneStatus
	if z := m.zoneState[zid]; z != nil {
		zs = *z
	}
	m.mu.RUnlock()
	switch {
	case h == "":
		return "no dashboard domain is set"
	case m.Certs.Get(h) == nil:
		return "waiting for its DNS record and Origin CA certificate"
	case m.cfg.AOP && !enforced:
		return "waiting for Cloudflare to activate authenticated origin pulls for " + zoneName(zs, h)
	case seen:
		// A request already came through Cloudflare with our AOP
		// certificate: the zone and its SSL/TLS mode work, whatever the
		// API says or whether the token may read the mode.
		return ""
	case zs.Status != "" && zs.Status != "active":
		return fmt.Sprintf("the zone %s is %q at Cloudflare, not active: finish adding the domain to Cloudflare (switch its nameservers)", zoneName(zs, h), zs.Status)
	case zs.SSLMode == "" && zs.Warning != "":
		return zs.Warning + ", or open https://" + h + "/ once: a visit through Cloudflare proves it works"
	case zs.SSLMode == "":
		return "checking the SSL/TLS mode of " + zoneName(zs, h) + "; opening https://" + h + "/ once also proves it works"
	case zs.SSLMode != "full" && zs.SSLMode != "strict":
		return fmt.Sprintf("the SSL/TLS mode of %s is %q, so Cloudflare cannot reach dootd; set it to Full (strict) (Settings → Zones)", zoneName(zs, h), zs.SSLMode)
	}
	return ""
}

// DashboardReached records that a request for the dashboard domain came
// through Cloudflare with a verified AOP client certificate (called by the
// router). It is remembered across restarts, per domain.
func (m *Manager) DashboardReached(host string) {
	m.mu.Lock()
	if host != m.dashHost || m.dashSeen == host {
		m.mu.Unlock()
		return
	}
	m.dashSeen = host
	m.mu.Unlock()
	m.log.Info("dashboard domain reached through Cloudflare", "domain", host)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.SetSetting(ctx, settingDashboardReached, []byte(host)); err != nil {
		m.log.Warn("saving the dashboard domain state", "err", err)
	}
}

func zoneName(zs ZoneStatus, host string) string {
	if zs.Name != "" {
		return zs.Name
	}
	return "the zone of " + host
}

// SetupOpen reports whether the setup address accepts connections: until
// the dashboard domain is ready, and while a one-time password is pending.
func (m *Manager) SetupOpen() bool {
	return !m.DashboardReady() || (m.cfg.SetupPending != nil && m.cfg.SetupPending())
}

// SetDashboardHost sets the dashboard domain (Req 7.2): it needs the
// Cloudflare token, must not be an app's domain, is routed at once and set
// up in the background. The old domain's DNS records and certificate are
// removed.
func (m *Manager) SetDashboardHost(ctx context.Context, host string) error {
	host = normalizeHost(strings.TrimSpace(host))
	if err := ValidHostname(host); err != nil {
		return err
	}
	if _, err := m.token(ctx); err != nil {
		return errors.New("save the Cloudflare token first")
	}
	if err := m.CheckZone(ctx, host); err != nil {
		return err
	}
	m.mu.Lock()
	old := m.dashHost
	for _, r := range m.routes {
		if r.Host == host {
			m.mu.Unlock()
			return fmt.Errorf("%s is the domain of the app %s", host, r.App)
		}
	}
	m.mu.Unlock()
	if host == old {
		m.SyncInBackground()
		return nil
	}
	if err := m.store.SetSetting(ctx, SettingDashboardDomain, []byte(host)); err != nil {
		return err
	}
	m.mu.Lock()
	m.dashHost = host
	m.mu.Unlock()
	m.Router.SetDashboardHost(host)
	m.rebuildHosts()
	m.log.Info("dashboard domain set", "domain", host, "previous", old)
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if old != "" {
			if err := m.RemoveHost(bctx, old); err != nil {
				m.log.Warn("cleaning up the old dashboard domain", "domain", old, "err", err)
			}
		}
		if _, err := m.Sync(bctx); err != nil {
			m.log.Warn("cloudflare sync failed", "err", err)
		}
	}()
	return nil
}

func (m *Manager) loadState(ctx context.Context) error {
	rows, err := m.store.Reader().QueryContext(ctx, `SELECT zone_id, name, ssl_mode, aop_cert_id, aop_cert_serial, aop_active FROM edge_zones`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		z := &ZoneStatus{}
		var active int
		if err := rows.Scan(&z.ID, &z.Name, &z.SSLMode, &z.AOPCertID, &z.AOPSerial, &active); err != nil {
			return err
		}
		// Enforce only if Cloudflare holds the client certificate we have now.
		on := m.cfg.AOP && active == 1 && z.AOPSerial == m.aop.Client.SerialNumber.String()
		m.enforce[z.ID] = on
		z.AOP = aopLabel(m.cfg.AOP, on)
		m.zoneState[z.ID] = z
	}
	if err := rows.Err(); err != nil {
		return err
	}
	crow, err := m.store.Reader().QueryContext(ctx, `SELECT hostname, zone_id FROM edge_certs`)
	if err != nil {
		return err
	}
	defer crow.Close()
	for crow.Next() {
		var h, z string
		if err := crow.Scan(&h, &z); err != nil {
			return err
		}
		m.hostZone[h] = z
	}
	return crow.Err()
}

func aopLabel(configured, enforced bool) string {
	switch {
	case !configured:
		return "off"
	case enforced:
		return "enforced"
	}
	return "pending"
}

// SetRoutes sets the app routes (and the hostnames to manage).
func (m *Manager) SetRoutes(routes []Route) {
	m.Router.SetRoutes(routes)
	m.mu.Lock()
	m.routes = append([]Route(nil), routes...)
	m.mu.Unlock()
	m.rebuildHosts()
}

func (m *Manager) rebuildHosts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hs []hostEntry
	if m.dashHost != "" {
		hs = append(hs, hostEntry{Host: m.dashHost})
	}
	for _, r := range m.routes {
		hs = append(hs, hostEntry{Host: r.Host, App: r.App})
	}
	m.hosts = hs
}

// tlsConfig picks the certificate by SNI and requires the AOP client
// certificate for zones where it is active.
func (m *Manager) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if _, ok := hello.Conn.(*setupConn); ok {
				// Setup address: self-signed, whatever the server name.
				return &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{*m.setupCert},
					NextProtos:   []string{"h2", "http/1.1"},
				}, nil
			}
			host := normalizeHost(hello.ServerName)
			cert := m.Certs.Get(host)
			if cert == nil {
				return nil, fmt.Errorf("edge: no certificate for server name %q", hello.ServerName)
			}
			c := &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{*cert},
				NextProtos:   []string{"h2", "http/1.1"},
			}
			m.mu.RLock()
			enforce := m.cfg.AOP && m.enforce[m.hostZone[host]]
			pool := m.aop.Pool
			m.mu.RUnlock()
			// No client certificate is asked for while Cloudflare rolls
			// ours out (D43): edge servers that don't have it yet present
			// Cloudflare's shared origin-pull certificate instead, which
			// would fail verification.
			if enforce {
				c.ClientAuth = tls.RequireAndVerifyClientCert
				c.ClientCAs = pool
			}
			return c, nil
		},
	}
}

// Serve listens until ctx ends.
func (m *Manager) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", m.cfg.Listen)
	if err != nil {
		return fmt.Errorf("edge: listen %s: %w", m.cfg.Listen, err)
	}
	fl := m.Filter.Listen(ln, m.log, m.SetupOpen)
	m.Router.SetupOpen = m.SetupOpen
	m.Router.DashboardReached = m.DashboardReached
	m.Router.SetDashboardHost(m.DashboardHost())
	srv := &http.Server{
		Handler:           m.Router,
		ConnContext:       connContext,
		TLSConfig:         m.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          stdlog.New(&serverErrors{log: m.log}, "", 0),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	m.log.Info("listening", "addr", m.cfg.Listen, "certificates", len(m.Certs.Hosts()))
	err = srv.ServeTLS(fl, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Run syncs with Cloudflare now and every 6 hours, and refreshes the IP
// ranges every 24 hours, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	go func() {
		m.refreshIPs(ctx)
		if _, err := m.Sync(ctx); err != nil {
			m.log.Warn("cloudflare sync failed", "err", err)
		}
		t := time.NewTicker(syncEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.mu.RLock()
				due := time.Since(m.lastIPs) >= ipRefreshEvery
				m.mu.RUnlock()
				if due {
					m.refreshIPs(ctx)
				}
				if _, err := m.Sync(ctx); err != nil {
					m.log.Warn("cloudflare sync failed", "err", err)
				}
			}
		}
	}()
}

func (m *Manager) client(token string) *cloudflare.Client {
	return &cloudflare.Client{Token: token, Base: m.cfg.APIBase}
}

func (m *Manager) refreshIPs(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := m.client("").IPs(ctx)
	if err != nil {
		m.log.Warn("refreshing Cloudflare IP ranges failed; keeping the current list", "err", err)
		return
	}
	all := append(append([]string{}, r.IPv4...), r.IPv6...)
	if err := m.Filter.Set(all, "cloudflare"); err != nil {
		m.log.Warn("Cloudflare returned an unusable IP range list; keeping the current list", "err", err)
		return
	}
	b, _ := json.Marshal(all)
	if err := m.store.SetSetting(ctx, settingCFRanges, b); err != nil {
		m.log.Warn("saving IP ranges", "err", err)
	}
	m.mu.Lock()
	m.lastIPs = time.Now()
	m.mu.Unlock()
	m.log.Info("Cloudflare IP ranges refreshed", "count", len(all))
}

func (m *Manager) token(ctx context.Context) (string, error) {
	v, ok, err := m.store.GetSetting(ctx, SettingCFToken)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("no Cloudflare API token set (Settings → Cloudflare)")
	}
	b, err := m.box.Open(v, purposeCFToken)
	if err != nil {
		return "", fmt.Errorf("stored Cloudflare token: %w", err)
	}
	return string(b), nil
}

// SetToken verifies and stores the Cloudflare API token (sealed). It
// returns the zone names the token can read and the permissions it
// appears to lack (Req 7.1), found by reading one zone.
func (m *Manager) SetToken(ctx context.Context, token string) (zones, missing []string, err error) {
	c := m.client(token)
	if err := c.Verify(ctx); err != nil {
		return nil, nil, err
	}
	zs, err := c.Zones(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(zs) > 0 {
		missing = missingPermissions(ctx, c, zs[0])
	}
	sealed, err := m.box.Seal([]byte(token), purposeCFToken)
	if err != nil {
		return nil, nil, err
	}
	if err := m.store.SetSetting(ctx, SettingCFToken, sealed); err != nil {
		return nil, nil, err
	}
	for _, z := range zs {
		zones = append(zones, z.Name)
	}
	sort.Strings(zones)
	return zones, missing, nil
}

// missingPermissions probes read calls that need the token's other
// permissions. A 401/403 means the permission is missing; other errors
// (network) say nothing and are ignored.
func missingPermissions(ctx context.Context, c *cloudflare.Client, z cloudflare.Zone) []string {
	var missing []string
	if _, err := c.DNSRecords(ctx, z.ID, z.Name); cloudflare.IsForbidden(err) {
		missing = append(missing, "Zone → DNS → Edit")
	}
	if _, err := c.ListAOPCerts(ctx, z.ID); cloudflare.IsForbidden(err) {
		missing = append(missing, "Zone → SSL and Certificates → Edit")
	}
	if _, err := c.SSLMode(ctx, z.ID); cloudflare.IsForbidden(err) {
		missing = append(missing, "Zone → Zone Settings → Edit")
	}
	return missing
}

// Sync reconciles DNS records, certificates, AOP and SSL mode checks.
func (m *Manager) Sync(ctx context.Context) (Status, error) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	err := m.sync(ctx)
	m.mu.Lock()
	m.lastSync = time.Now()
	m.lastErr = ""
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
	return m.Status(ctx), err
}

func (m *Manager) sync(ctx context.Context) error {
	token, err := m.token(ctx)
	if err != nil {
		return err
	}
	c := m.client(token)

	aop, rotated, err := LoadOrCreateAOP(filepath.Join(m.cfg.DataRoot, "aop"))
	if err != nil {
		return err
	}
	if rotated {
		m.log.Info("AOP client certificate rotated", "not_after", aop.Client.NotAfter)
	}
	m.mu.Lock()
	m.aop = aop
	hosts := append([]hostEntry{}, m.hosts...)
	m.mu.Unlock()

	v4, v6, err := m.publicIPs(ctx)
	if err != nil {
		return err
	}

	var errs []error
	zones := map[string]cloudflare.Zone{}
	for _, h := range hosts {
		hs := &HostStatus{Host: h.Host, App: h.App}
		if err := m.syncHost(ctx, c, h, v4, v6, hs, zones); err != nil {
			hs.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", h.Host, err))
		}
		m.mu.Lock()
		m.hostState[h.Host] = hs
		m.mu.Unlock()
	}
	for _, z := range zones {
		if err := m.syncZone(ctx, c, z, aop); err != nil {
			errs = append(errs, fmt.Errorf("zone %s: %w", z.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) syncHost(ctx context.Context, c *cloudflare.Client, h hostEntry, v4, v6 string, hs *HostStatus, zones map[string]cloudflare.Zone) error {
	z, err := m.zoneFor(ctx, c, h.Host, zones)
	if err != nil {
		return err
	}
	hs.Zone = z.Name
	m.mu.Lock()
	m.hostZone[h.Host] = z.ID
	m.mu.Unlock()

	var dns []string
	if v4 != "" {
		res, err := c.UpsertProxied(ctx, z.ID, h.Host, "A", v4)
		if err != nil {
			return fmt.Errorf("DNS: %w", err)
		}
		dns = append(dns, "A "+v4+" ("+res+")")
	}
	if v6 != "" {
		res, err := c.UpsertProxied(ctx, z.ID, h.Host, "AAAA", v6)
		if err != nil {
			return fmt.Errorf("DNS: %w", err)
		}
		dns = append(dns, "AAAA "+v6+" ("+res+")")
	}
	hs.DNS = strings.Join(dns, ", ")

	if need, why := m.Certs.NeedsCert(h.Host, certRenewBefore); need {
		m.log.Info("issuing Origin CA certificate", "host", h.Host, "reason", why)
		if err := m.issue(ctx, c, h.Host, z.ID); err != nil {
			return fmt.Errorf("certificate: %w", err)
		}
	}
	if l := m.Certs.Leaf(h.Host); l != nil {
		hs.CertUntil = l.NotAfter
	}
	return nil
}

func (m *Manager) zoneFor(ctx context.Context, c *cloudflare.Client, host string, cache map[string]cloudflare.Zone) (cloudflare.Zone, error) {
	for name, z := range cache {
		if host == name || strings.HasSuffix(host, "."+name) {
			return z, nil
		}
	}
	z, err := c.ZoneFor(ctx, host)
	if err != nil {
		return z, err
	}
	cache[z.Name] = z
	return z, nil
}

func (m *Manager) issue(ctx context.Context, c *cloudflare.Client, host, zoneID string) error {
	csr, key, err := newCSR(host)
	if err != nil {
		return err
	}
	oc, err := c.CreateOriginCert(ctx, string(csr), []string{host}, certDays)
	if err != nil {
		return err
	}
	leaf, err := m.Certs.Put(host, []byte(oc.Certificate), key)
	if err != nil {
		return err
	}
	var oldID string
	err = m.store.Reader().QueryRowContext(ctx, `SELECT cf_cert_id FROM edge_certs WHERE hostname = ?`, host).Scan(&oldID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := m.store.Writer().ExecContext(ctx, `INSERT INTO edge_certs (hostname, zone_id, cf_cert_id, not_after, issued_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (hostname) DO UPDATE SET zone_id = excluded.zone_id,
		cf_cert_id = excluded.cf_cert_id, not_after = excluded.not_after, issued_at = excluded.issued_at`,
		host, zoneID, oc.ID, leaf.NotAfter.Unix(), time.Now().Unix()); err != nil {
		return err
	}
	if oldID != "" && oldID != oc.ID {
		if err := c.RevokeOriginCert(ctx, oldID); err != nil && !cloudflare.IsNotFound(err) {
			m.log.Warn("revoking the replaced Origin CA certificate failed", "host", host, "id", oldID, "err", err)
		}
	}
	m.log.Info("certificate installed", "host", host, "not_after", leaf.NotAfter.Format(time.DateOnly))
	return nil
}

func (m *Manager) syncZone(ctx context.Context, c *cloudflare.Client, z cloudflare.Zone, aop *AOP) error {
	m.mu.RLock()
	prev := m.zoneState[z.ID]
	m.mu.RUnlock()
	zs := &ZoneStatus{ID: z.ID, Name: z.Name}
	if prev != nil {
		zs.AOPCertID, zs.AOPSerial = prev.AOPCertID, prev.AOPSerial
	}
	var errs []error

	zs.Status = z.Status
	if z.Status != "" && z.Status != "active" {
		zs.Warning = fmt.Sprintf("the zone is %q at Cloudflare, not active: its sites cannot be reached until the domain's nameservers point to Cloudflare", z.Status)
	}
	mode, err := c.SSLMode(ctx, z.ID)
	if err != nil {
		errs = append(errs, err)
		// Keep the last mode we read: an API failure must not change
		// whether the dashboard domain counts as ready.
		if prev != nil {
			mode = prev.SSLMode
		}
		if cloudflare.IsForbidden(err) && mode == "" {
			zs.Warning = "dootd cannot read the SSL/TLS mode: the Cloudflare token lacks the Zone → Zone Settings → Edit permission. " +
				"Edit the token in Cloudflare (My Profile → API Tokens) to add it, then press Sync now"
		}
	}
	zs.SSLMode = mode
	switch mode {
	case "", "strict":
	case "full":
		zs.Warning = fmt.Sprintf("SSL/TLS mode is %q; use Full (strict) so Cloudflare verifies dootd's certificate (Settings → Zones → Set Full (strict))", mode)
	default: // flexible, off
		zs.Warning = fmt.Sprintf("SSL/TLS mode is %q: Cloudflare connects to this server over plain HTTP, which dootd does not serve, "+
			"so sites in this zone fail with error 521. Set it to Full (strict) (Settings → Zones → Set Full (strict))", mode)
	}

	enforced := false
	if m.cfg.AOP {
		ready, changed, err := m.syncAOP(ctx, c, z, aop, zs)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("authenticated origin pulls: %w", err))
			// Keep enforcing if it was on and the API is merely failing.
			if prev != nil && m.enforceFor(z.ID) && zs.AOPSerial == aop.Client.SerialNumber.String() {
				enforced = true
			}
		case ready:
			enforced = m.rolledOut(z.ID, changed)
		}
	}
	zs.AOP = aopLabel(m.cfg.AOP, enforced)
	m.mu.RLock()
	if _, rolling := m.rollout[z.ID]; rolling && !enforced {
		zs.AOP = "rolling out"
	}
	m.mu.RUnlock()
	if len(errs) > 0 {
		zs.Error = errors.Join(errs...).Error()
	}
	active := 0
	if enforced {
		active = 1
	}
	if _, err := m.store.Writer().ExecContext(ctx, `INSERT INTO edge_zones (zone_id, name, ssl_mode, aop_cert_id, aop_cert_serial, aop_active, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (zone_id) DO UPDATE SET name = excluded.name, ssl_mode = excluded.ssl_mode,
		aop_cert_id = excluded.aop_cert_id, aop_cert_serial = excluded.aop_cert_serial, aop_active = excluded.aop_active,
		checked_at = excluded.checked_at`, z.ID, z.Name, zs.SSLMode, zs.AOPCertID, zs.AOPSerial, active, time.Now().Unix()); err != nil {
		errs = append(errs, err)
	}
	m.mu.Lock()
	m.zoneState[z.ID] = zs
	m.enforce[z.ID] = enforced
	m.mu.Unlock()
	return errors.Join(errs...)
}

func (m *Manager) enforceFor(zoneID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enforce[zoneID]
}

// syncAOP makes sure Cloudflare holds our current client certificate for
// the zone and has zone-level AOP enabled. It returns whether both are in
// place at Cloudflare (ready) and whether this call had to upload the
// certificate or turn AOP on (changed), which Cloudflare's edge then needs
// time to roll out (see rolledOut).
func (m *Manager) syncAOP(ctx context.Context, c *cloudflare.Client, z cloudflare.Zone, aop *AOP, zs *ZoneStatus) (ready, changed bool, err error) {
	serial := aop.Client.SerialNumber.String()
	if zs.AOPCertID != "" && zs.AOPSerial == serial {
		cur, err := c.AOPCert(ctx, z.ID, zs.AOPCertID)
		switch {
		case cloudflare.IsNotFound(err), err == nil && (cur.Status == "deleted" || cur.Status == "pending_deletion"):
			m.log.Warn("our AOP certificate is gone from Cloudflare; uploading it again", "zone", z.Name)
			zs.AOPCertID = ""
		case err != nil:
			return false, false, err
		}
	}
	oldID := ""
	if zs.AOPCertID == "" || zs.AOPSerial != serial {
		oldID = zs.AOPCertID
		up, err := c.UploadAOPCert(ctx, z.ID, string(aop.ClientPEM), string(aop.ClientKey))
		if err != nil {
			return false, false, err
		}
		zs.AOPCertID, zs.AOPSerial = up.ID, serial
		changed = true
		m.log.Info("uploaded AOP client certificate", "zone", z.Name, "id", up.ID)
	}
	if err := m.waitAOPActive(ctx, c, z.ID, zs.AOPCertID); err != nil {
		return false, changed, err
	}
	on, err := c.AOPEnabled(ctx, z.ID)
	if err != nil {
		return false, changed, err
	}
	if !on {
		if err := c.SetAOPEnabled(ctx, z.ID, true); err != nil {
			return false, changed, err
		}
		changed = true
		m.log.Info("enabled zone-level authenticated origin pulls", "zone", z.Name)
	}
	if oldID != "" && oldID != zs.AOPCertID {
		if err := c.DeleteAOPCert(ctx, z.ID, oldID); err != nil && !cloudflare.IsNotFound(err) {
			m.log.Warn("deleting the previous AOP certificate failed", "zone", z.Name, "id", oldID, "err", err)
		}
	}
	return true, changed, nil
}

// rolledOut decides whether to require client certificates for a zone
// whose AOP setup is in place at Cloudflare. A zone that is enforced
// already stays enforced, unless the sync just uploaded the certificate or
// turned AOP on: then, and for a zone that was never enforced, dootd
// waits AOPRollout for Cloudflare's edge to catch up, accepting
// connections without a certificate meanwhile (the IP filter stays on),
// and syncs again when the time is up.
func (m *Manager) rolledOut(zoneID string, changed bool) bool {
	wait := m.cfg.AOPRollout
	if wait == 0 {
		wait = AOPRollout
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	since, pending := m.rollout[zoneID]
	if !changed && !pending && m.enforce[zoneID] {
		return true
	}
	if changed || !pending {
		since = now
	}
	left := wait - now.Sub(since)
	if left <= 0 {
		delete(m.rollout, zoneID)
		return true
	}
	if !pending || changed {
		m.log.Info("waiting for Cloudflare to roll out authenticated origin pulls before requiring them", "zone_id", zoneID, "for", left.Round(time.Second))
	}
	m.rollout[zoneID] = since
	if t := m.rollTimer[zoneID]; t != nil {
		t.Stop()
	}
	m.rollTimer[zoneID] = time.AfterFunc(left+time.Second, m.SyncInBackground)
	return false
}

func (m *Manager) waitAOPActive(ctx context.Context, c *cloudflare.Client, zoneID, id string) error {
	deadline := time.Now().Add(aopActivateWait)
	for {
		cur, err := c.AOPCert(ctx, zoneID, id)
		if err != nil {
			return err
		}
		switch cur.Status {
		case "active":
			return nil
		case "deployment_timed_out", "deleted", "pending_deletion", "deletion_timed_out":
			return fmt.Errorf("client certificate %s is %s at Cloudflare", id, cur.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("client certificate %s still %q after %s; will check again on the next sync", id, cur.Status, aopActivateWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// SetStrict sets a zone's SSL/TLS mode to Full (strict).
func (m *Manager) SetStrict(ctx context.Context, zoneName string) error {
	token, err := m.token(ctx)
	if err != nil {
		return err
	}
	c := m.client(token)
	z, err := c.ZoneFor(ctx, zoneName)
	if err != nil {
		return err
	}
	if z.Name != zoneName {
		return fmt.Errorf("%s is not a zone (did you mean %s?)", zoneName, z.Name)
	}
	if err := c.SetSSLMode(ctx, z.ID, "strict"); err != nil {
		return err
	}
	m.log.Info("zone SSL/TLS mode set to Full (strict)", "zone", z.Name)
	// Refresh the status; errors about other hosts are not this call's failure.
	if _, err := m.Sync(ctx); err != nil {
		m.log.Warn("sync after set-strict reported errors", "err", err)
	}
	return nil
}

func (m *Manager) publicIPs(ctx context.Context) (string, string, error) {
	m.mu.RLock()
	v4, v6 := m.pubV4, m.pubV6
	m.mu.RUnlock()
	if v4 == "" {
		v4 = m.cfg.PublicIPv4
		if v4 == "" {
			ip, err := DetectIP(ctx, "tcp4")
			if err != nil {
				return "", "", fmt.Errorf("detecting the server's public IPv4 address failed: %w", err)
			}
			v4 = ip
		}
		if a, err := netip.ParseAddr(v4); err != nil || !a.Is4() {
			return "", "", fmt.Errorf("public IPv4 %q is not an IPv4 address", v4)
		}
	}
	if v6 == "" {
		switch m.cfg.PublicIPv6 {
		case "off":
			v6 = "off"
		case "":
			if ip, err := DetectIP(ctx, "tcp6"); err == nil {
				v6 = ip
			} else {
				v6 = "off"
			}
		default:
			if a, err := netip.ParseAddr(m.cfg.PublicIPv6); err != nil || !a.Is6() {
				return "", "", fmt.Errorf("public IPv6 %q is not an IPv6 address", m.cfg.PublicIPv6)
			}
			v6 = m.cfg.PublicIPv6
		}
	}
	m.mu.Lock()
	m.pubV4, m.pubV6 = v4, v6
	m.mu.Unlock()
	if v6 == "off" {
		v6 = ""
	}
	return v4, v6, nil
}

// DetectIP asks Cloudflare's trace endpoint which address we come from.
func DetectIP(ctx context.Context, network string) (string, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return d.DialContext(ctx, network, addr) },
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "ip="); ok {
			if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil && a.IsGlobalUnicast() && !a.IsPrivate() {
				return a.String(), nil
			}
			return "", fmt.Errorf("trace returned a non-public address %q", v)
		}
	}
	return "", errors.New("no ip= line in trace response")
}

// Status returns a snapshot.
func (m *Manager) Status(ctx context.Context) Status {
	_, tokErr := m.token(ctx)
	ranges, src := m.Filter.Ranges()
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Status{
		Listen: m.cfg.Listen, Dashboard: m.dashHost, TokenSet: tokErr == nil,
		PublicIPv4: m.pubV4, PublicIPv6: m.pubV6, IPRanges: len(ranges), IPSource: src,
		Rejected: m.Filter.Rejected(), LastSync: m.lastSync, LastError: m.lastErr,
		AOPClientTo: m.aop.Client.NotAfter,
	}
	for _, h := range m.hosts {
		if hs, ok := m.hostState[h.Host]; ok {
			s.Hosts = append(s.Hosts, *hs)
			continue
		}
		hs := HostStatus{Host: h.Host, App: h.App, DNS: "not synced yet"}
		if l := m.Certs.Leaf(h.Host); l != nil {
			hs.CertUntil = l.NotAfter
		}
		s.Hosts = append(s.Hosts, hs)
	}
	for _, z := range m.zoneState {
		s.Zones = append(s.Zones, *z)
	}
	sort.Slice(s.Zones, func(i, j int) bool { return s.Zones[i].Name < s.Zones[j].Name })
	return s
}

// serverErrors receives net/http's own error lines (mostly failed TLS
// handshakes, e.g. Cloudflare connecting without the AOP client
// certificate) and logs them at most once a minute with a count, so such a
// problem shows in the journal without flooding it.
type serverErrors struct {
	log    *slog.Logger
	mu     sync.Mutex
	n      int
	latest string
	timer  *time.Timer // set while a one-minute window is open
}

// Write logs the first error at once, then counts the rest and logs a
// summary every minute while they continue.
func (e *serverErrors) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.latest = strings.TrimSpace(string(p))
	if e.timer != nil {
		e.n++
		return len(p), nil
	}
	e.log.Warn("a connection failed before a request was read", "err", e.latest)
	e.timer = time.AfterFunc(time.Minute, e.flush)
	return len(p), nil
}

func (e *serverErrors) flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == 0 {
		e.timer = nil
		return
	}
	e.log.Warn("more connections failed before a request was read", "count", e.n, "in", "1m", "latest", e.latest)
	e.n = 0
	e.timer = time.AfterFunc(time.Minute, e.flush)
}

// CheckZone reports a hostname that no zone of the Cloudflare account
// serves (a typo, or a domain on another account), so the form can say so
// at once instead of the background sync failing later. Without a token,
// or when the API cannot be reached, it reports nothing: the sync shows
// those problems.
func (m *Manager) CheckZone(ctx context.Context, host string) error {
	token, err := m.token(ctx)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := m.client(token).ZoneFor(ctx, host); errors.Is(err, cloudflare.ErrNoZone) {
		return err
	}
	return nil
}

// SyncInBackground starts a sync (e.g. after an app or domain was added)
// without waiting for it.
func (m *Manager) SyncInBackground() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := m.Sync(ctx); err != nil {
			m.log.Warn("cloudflare sync failed", "err", err)
		}
	}()
}

// RemoveHost cleans up a hostname that is no longer served: it deletes the
// A/AAAA records that point at this server, revokes the Origin CA
// certificate and removes the local copy. AOP stays on for the zone.
func (m *Manager) RemoveHost(ctx context.Context, host string) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	var errs []error
	m.mu.Lock()
	zoneID := m.hostZone[host]
	delete(m.hostZone, host)
	delete(m.hostState, host)
	v4, v6 := m.pubV4, m.pubV6
	m.mu.Unlock()

	var certID string
	m.store.Reader().QueryRowContext(ctx, `SELECT cf_cert_id FROM edge_certs WHERE hostname = ?`, host).Scan(&certID)
	if token, err := m.token(ctx); err == nil && zoneID != "" {
		c := m.client(token)
		if rs, err := c.DNSRecords(ctx, zoneID, host); err != nil {
			errs = append(errs, fmt.Errorf("DNS: %w", err))
		} else {
			for _, r := range rs {
				if (r.Type == "A" && r.Content == v4) || (r.Type == "AAAA" && r.Content == v6) {
					if err := c.DeleteDNSRecord(ctx, zoneID, r.ID); err != nil {
						errs = append(errs, fmt.Errorf("DNS: %w", err))
					}
				}
			}
		}
		if certID != "" {
			if err := c.RevokeOriginCert(ctx, certID); err != nil && !cloudflare.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("revoke certificate: %w", err))
			}
		}
	} else if err != nil {
		errs = append(errs, fmt.Errorf("DNS record and certificate left in Cloudflare: %w", err))
	}
	if _, err := m.store.Writer().ExecContext(ctx, `DELETE FROM edge_certs WHERE hostname = ?`, host); err != nil {
		errs = append(errs, err)
	}
	m.Certs.Delete(host)
	if len(errs) == 0 {
		m.log.Info("removed hostname", "host", host)
	}
	return errors.Join(errs...)
}
