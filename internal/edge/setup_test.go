package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumitwaani2/dootd/internal/secrets"
	"github.com/sumitwaani2/dootd/internal/store"
)

// TestSetupAddress runs the real listener on loopback. 127.0.0.1 is not a
// Cloudflare address, so every connection is a direct one.
func TestSetupAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "dootd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := secrets.LoadOrCreate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	var pending atomic.Bool
	pending.Store(true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(ctx, Config{Listen: addr, AOP: true, DataRoot: dir, SetupPending: pending.Load}, st, box, log)
	if err != nil {
		t.Fatal(err)
	}
	var sawIP atomic.Value
	m.Router = &Router{Log: log, State: func(string) Availability { return Available },
		Dashboard: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawIP.Store(ClientIP(r))
			io.WriteString(w, "dashboard")
		})}
	m.SetRoutes([]Route{{Host: "app.example.test", App: "app", Port: 1}})
	go m.Serve(ctx)

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	get := func(url string) (int, string, *tls.ConnectionState, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("CF-Connecting-IP", "203.0.113.99") // forged: must be ignored
		resp, err := c.Do(req)
		if err != nil {
			return 0, "", nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.TLS, nil
	}
	var code int
	var body string
	var cs *tls.ConnectionState
	for i := 0; i < 50; i++ {
		if code, body, cs, err = get("https://203.0.113.5/"); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || code != 200 || body != "dashboard" {
		t.Fatalf("setup open: %d %q %v", code, body, err)
	}
	if cs.PeerCertificates[0].Subject.CommonName != "dootd setup" {
		t.Fatalf("certificate %v", cs.PeerCertificates[0].Subject)
	}
	if ip := sawIP.Load(); ip != "127.0.0.1" {
		t.Fatalf("client IP on the setup address = %v, want the TCP peer", ip)
	}
	// An app's domain is never served on a setup connection.
	if _, body, _, _ = get("https://app.example.test/"); body != "dashboard" {
		t.Fatalf("app domain on the setup address: %q", body)
	}

	// Close setup: dashboard domain ready (certificate + AOP enforced) and
	// no one-time password pending.
	m.mu.Lock()
	m.dashHost = "dootd.example.test"
	m.mu.Unlock()
	m.Router.SetDashboardHost("dootd.example.test")
	certPEM, keyPEM := selfSigned(t, "dootd.example.test")
	if _, err := m.Certs.Put("dootd.example.test", certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	zone := &ZoneStatus{ID: "z1", Name: "example.test"}
	m.mu.Lock()
	m.hostZone["dootd.example.test"] = "z1"
	m.enforce["z1"] = true
	m.zoneState["z1"] = zone
	m.mu.Unlock()
	if m.SetupOpen() != true {
		t.Fatal("a pending one-time password keeps setup open")
	}
	pending.Store(false)
	// Cloudflare cannot reach :443 unless the zone is in Full or Full
	// (strict) mode, and an inactive zone serves nothing: setup stays open.
	for _, c := range []struct{ mode, status, want string }{
		{"", "", "checking the SSL/TLS mode"},
		{"flexible", "active", `"flexible"`},
		{"off", "", `"off"`},
		{"strict", "pending", `"pending"`},
	} {
		m.mu.Lock()
		zone.SSLMode, zone.Status = c.mode, c.status
		m.mu.Unlock()
		if !m.SetupOpen() || !strings.Contains(m.DashboardProblem(), c.want) {
			t.Fatalf("mode %q status %q: setup open %v, problem %q", c.mode, c.status, m.SetupOpen(), m.DashboardProblem())
		}
	}
	m.mu.Lock()
	zone.SSLMode, zone.Status = "strict", "active"
	m.mu.Unlock()
	if m.SetupOpen() {
		t.Fatal("setup should be closed: " + m.DashboardProblem())
	}
	// A visit through Cloudflare with AOP proves the domain works even when
	// the mode cannot be read (token without Zone Settings) or a per-host
	// rule overrides a Flexible zone. It is remembered, per domain.
	m.mu.Lock()
	zone.SSLMode, zone.Warning = "", "dootd cannot read the SSL/TLS mode"
	m.mu.Unlock()
	if !m.SetupOpen() {
		t.Fatal("unknown SSL mode: setup should stay open")
	}
	m.DashboardReached("other.example.test") // not the dashboard domain: ignored
	if !m.SetupOpen() {
		t.Fatal("a visit to another domain must not close setup")
	}
	m.DashboardReached("dootd.example.test")
	if m.SetupOpen() {
		t.Fatal("setup should be closed after a visit through Cloudflare: " + m.DashboardProblem())
	}
	if v, ok, _ := st.GetSetting(ctx, settingDashboardReached); !ok || string(v) != "dootd.example.test" {
		t.Fatalf("reached state not saved: %q", v)
	}
	// A connection that was already open gets a pointer to the domain.
	code, body, _, err = get("https://203.0.113.5/")
	if err != nil || code != http.StatusForbidden || !strings.Contains(body, "https://dootd.example.test/") {
		t.Fatalf("old connection after setup closed: %d %q %v", code, body, err)
	}
	// New direct connections are closed before TLS.
	tr.CloseIdleConnections()
	if _, _, _, err := get("https://203.0.113.5/"); err == nil {
		t.Fatal("new direct connection accepted after setup closed")
	}
	if m.Filter.Rejected() == 0 {
		t.Fatal("rejection not counted")
	}
}

func selfSigned(t *testing.T, host string) ([]byte, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}
