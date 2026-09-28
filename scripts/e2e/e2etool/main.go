// Command e2etool supports the end-to-end scripts. It is not part of dootd.
//
//	e2etool cfmock  -listen ADDR -dir DIR -token T -zones a.test,b.test [-extra-range CIDR] [-short-first]
//	    A fake Cloudflare API: zones, DNS, Origin CA (signs CSRs with its own
//	    root, written to DIR/origin-ca.pem), SSL mode, zone-level AOP (the
//	    uploaded client cert/key are written to DIR/aop-client.{pem,key}, the
//	    way Cloudflare's edge would present them) and /ips. GET /_mock/state
//	    dumps everything as JSON.
//	e2etool echo [-delay D]
//	    An app following the dootd contract that echoes request details as
//	    JSON, streams on /stream and echoes raw bytes after an Upgrade on
//	    /upgrade.
//	e2etool upgrade -addr IP:443 -host H -ca F [-cert F -key F]
//	    Performs an HTTP Upgrade through dootd over TLS and checks the echo.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: e2etool cfmock|echo|upgrade ...")
	}
	switch os.Args[1] {
	case "cfmock":
		cfmock(os.Args[2:])
	case "echo":
		echo(os.Args[2:])
	case "upgrade":
		upgrade(os.Args[2:])
	default:
		log.Fatalf("unknown subcommand %q", os.Args[1])
	}
}

// ---------------------------------------------------------------- cfmock

type mockDNS struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

type mockAOP struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Serial string `json:"serial"`
	gets   int
}

type mockZone struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	SSL        string              `json:"ssl"`
	AOPEnabled bool                `json:"aop_enabled"`
	DNS        map[string]*mockDNS `json:"dns"`
	AOP        map[string]*mockAOP `json:"aop"`
}

type mockCert struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	NotAfter time.Time `json:"not_after"`
	Revoked  bool      `json:"revoked"`
}

type mock struct {
	mu     sync.Mutex
	token  string
	dir    string
	extra  []string
	short  bool
	zones  map[string]*mockZone // by id
	certs  map[string]*mockCert
	issued map[string]int // host -> count
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func randID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func cfmock(args []string) {
	fs := flag.NewFlagSet("cfmock", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8787", "")
	dir := fs.String("dir", ".", "")
	token := fs.String("token", "e2e-token", "")
	zones := fs.String("zones", "example.test", "")
	extra := fs.String("extra-range", "", "extra CIDR returned by /ips")
	short := fs.Bool("short-first", false, "first certificate per host is valid for only 10 days")
	fs.Parse(args)

	m := &mock{token: *token, dir: *dir, short: *short, zones: map[string]*mockZone{}, certs: map[string]*mockCert{}, issued: map[string]int{}}
	if *extra != "" {
		m.extra = []string{*extra}
	}
	for _, z := range strings.Split(*zones, ",") {
		id := "z-" + strings.ReplaceAll(z, ".", "-")
		m.zones[id] = &mockZone{ID: id, Name: z, SSL: "full", DNS: map[string]*mockDNS{}, AOP: map[string]*mockAOP{}}
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Mock Cloudflare Origin CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(20, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	m.caCert, _ = x509.ParseCertificate(der)
	m.caKey = k
	os.MkdirAll(*dir, 0o755)
	os.WriteFile(filepath.Join(*dir, "origin-ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)

	mux := http.NewServeMux()
	p := "/client/v4"
	mux.HandleFunc("GET "+p+"/ips", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{
			"ipv4_cidrs": append([]string{"173.245.48.0/20", "103.21.244.0/22", "104.16.0.0/13", "172.64.0.0/13"}, m.extra...),
			"ipv6_cidrs": []string{"2400:cb00::/32", "2606:4700::/32"},
		})
	})
	mux.HandleFunc("GET /_mock/state", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"zones": m.zones, "certs": m.certs, "issued": m.issued})
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+m.token {
				fail(w, http.StatusUnauthorized, "Invalid API Token")
				return
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			h(w, r)
		}
	}
	zone := func(w http.ResponseWriter, r *http.Request) *mockZone {
		z := m.zones[r.PathValue("zone")]
		if z == nil {
			fail(w, http.StatusNotFound, "zone not found")
		}
		return z
	}
	mux.HandleFunc("GET "+p+"/user/tokens/verify", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]string{"status": "active"})
	}))
	mux.HandleFunc("GET "+p+"/zones", auth(func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]string{}
		for _, z := range m.zones {
			if n := r.URL.Query().Get("name"); n == "" || n == z.Name {
				out = append(out, map[string]string{"id": z.ID, "name": z.Name})
			}
		}
		ok(w, out)
	}))
	mux.HandleFunc("GET "+p+"/zones/{zone}/dns_records", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			out := []*mockDNS{}
			for _, d := range z.DNS {
				if n := r.URL.Query().Get("name"); n == "" || n == d.Name {
					out = append(out, d)
				}
			}
			ok(w, out)
		}
	}))
	mux.HandleFunc("POST "+p+"/zones/{zone}/dns_records", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			var d mockDNS
			json.NewDecoder(r.Body).Decode(&d)
			if !strings.HasSuffix(d.Name, "."+z.Name) && d.Name != z.Name {
				fail(w, http.StatusBadRequest, "record not in zone")
				return
			}
			d.ID = randID()
			z.DNS[d.ID] = &d
			ok(w, d)
		}
	}))
	mux.HandleFunc("PATCH "+p+"/zones/{zone}/dns_records/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			d := z.DNS[r.PathValue("id")]
			if d == nil {
				fail(w, http.StatusNotFound, "record not found")
				return
			}
			json.NewDecoder(r.Body).Decode(d)
			ok(w, d)
		}
	}))
	mux.HandleFunc("DELETE "+p+"/zones/{zone}/dns_records/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			if z.DNS[r.PathValue("id")] == nil {
				fail(w, http.StatusNotFound, "record not found")
				return
			}
			delete(z.DNS, r.PathValue("id"))
			ok(w, map[string]string{"id": r.PathValue("id")})
		}
	}))
	mux.HandleFunc("GET "+p+"/zones/{zone}/settings/ssl", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			ok(w, map[string]string{"id": "ssl", "value": z.SSL})
		}
	}))
	mux.HandleFunc("PATCH "+p+"/zones/{zone}/settings/ssl", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			var v struct{ Value string }
			json.NewDecoder(r.Body).Decode(&v)
			z.SSL = v.Value
			ok(w, map[string]string{"id": "ssl", "value": z.SSL})
		}
	}))
	mux.HandleFunc("POST "+p+"/certificates", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CSR       string   `json:"csr"`
			Hostnames []string `json:"hostnames"`
			Type      string   `json:"request_type"`
			Days      int      `json:"requested_validity"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		blk, _ := pem.Decode([]byte(req.CSR))
		if blk == nil || req.Type != "origin-ecc" || len(req.Hostnames) == 0 {
			fail(w, http.StatusBadRequest, "bad certificate request")
			return
		}
		csr, err := x509.ParseCertificateRequest(blk.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			fail(w, http.StatusBadRequest, "bad CSR")
			return
		}
		host := req.Hostnames[0]
		validity := time.Duration(req.Days) * 24 * time.Hour
		if m.short && m.issued[host] == 0 {
			validity = 10 * 24 * time.Hour
		}
		m.issued[host]++
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host},
			DNSNames: req.Hostnames, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validity),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, csr.PublicKey, m.caKey)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		c := &mockCert{ID: randID(), Host: host, NotAfter: tmpl.NotAfter}
		m.certs[c.ID] = c
		ok(w, map[string]string{"id": c.ID, "certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			"expires_on": tmpl.NotAfter.Format(time.RFC3339)})
	}))
	mux.HandleFunc("DELETE "+p+"/certificates/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		c := m.certs[r.PathValue("id")]
		if c == nil {
			fail(w, http.StatusNotFound, "certificate not found")
			return
		}
		c.Revoked = true
		ok(w, map[string]string{"id": c.ID})
	}))
	mux.HandleFunc("POST "+p+"/zones/{zone}/origin_tls_client_auth", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			var req struct {
				Certificate string `json:"certificate"`
				PrivateKey  string `json:"private_key"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			pair, err := tls.X509KeyPair([]byte(req.Certificate), []byte(req.PrivateKey))
			if err != nil {
				fail(w, http.StatusBadRequest, "certificate and key do not match: "+err.Error())
				return
			}
			os.WriteFile(filepath.Join(m.dir, "aop-client.pem"), []byte(req.Certificate), 0o644)
			os.WriteFile(filepath.Join(m.dir, "aop-client.key"), []byte(req.PrivateKey), 0o644)
			a := &mockAOP{ID: randID(), Status: "initializing", Serial: pair.Leaf.SerialNumber.String()}
			z.AOP[a.ID] = a
			ok(w, a)
		}
	}))
	mux.HandleFunc("GET "+p+"/zones/{zone}/origin_tls_client_auth", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			out := []*mockAOP{}
			for _, a := range z.AOP {
				out = append(out, a)
			}
			ok(w, out)
		}
	}))
	mux.HandleFunc("GET "+p+"/zones/{zone}/origin_tls_client_auth/settings", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			ok(w, map[string]bool{"enabled": z.AOPEnabled})
		}
	}))
	mux.HandleFunc("PUT "+p+"/zones/{zone}/origin_tls_client_auth/settings", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			var v struct{ Enabled bool }
			json.NewDecoder(r.Body).Decode(&v)
			z.AOPEnabled = v.Enabled
			ok(w, map[string]bool{"enabled": z.AOPEnabled})
		}
	}))
	mux.HandleFunc("GET "+p+"/zones/{zone}/origin_tls_client_auth/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			a := z.AOP[r.PathValue("id")]
			if a == nil {
				fail(w, http.StatusNotFound, "certificate not found")
				return
			}
			// Like Cloudflare, activation takes a moment.
			if a.gets++; a.gets >= 2 && a.Status == "initializing" {
				a.Status = "active"
			}
			ok(w, a)
		}
	}))
	mux.HandleFunc("DELETE "+p+"/zones/{zone}/origin_tls_client_auth/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if z := zone(w, r); z != nil {
			delete(z.AOP, r.PathValue("id"))
			ok(w, map[string]string{"id": r.PathValue("id")})
		}
	}))
	log.Printf("cfmock listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func ok(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1000, "message": msg}}})
}

// ---------------------------------------------------------------- echo

var keep [][]byte

func echo(args []string) {
	fs := flag.NewFlagSet("echo", flag.ExitOnError)
	delay := fs.Duration("delay", 0, "wait before listening (simulates a slow start)")
	fs.Parse(args)
	time.Sleep(*delay)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]string{}
		for k := range r.Header {
			h[k] = r.Header.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"app": os.Getenv("DOOTD_APP"), "release": os.Getenv("DOOTD_RELEASE"),
			"host": r.Host, "path": r.URL.Path, "remote": r.RemoteAddr, "headers": h})
	})
	// /burn?s=N keeps one core busy for N seconds (monitoring tests).
	mux.HandleFunc("/burn", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := time.ParseDuration(r.URL.Query().Get("s") + "s")
		end := time.Now().Add(secs)
		go func() {
			x := 0.0
			for time.Now().Before(end) {
				for i := 0; i < 1e5; i++ {
					x += float64(i) * 1.0000001
				}
			}
			_ = x
		}()
		io.WriteString(w, "burning\n")
	})
	// /alloc?mb=N allocates and keeps N MB (memory and OOM tests).
	mux.HandleFunc("/alloc", func(w http.ResponseWriter, r *http.Request) {
		var mb int
		fmt.Sscan(r.URL.Query().Get("mb"), &mb)
		b := make([]byte, mb<<20)
		for i := range b {
			b[i] = byte(i*7 + i>>9)
		}
		keep = append(keep, b)
		io.WriteString(w, "allocated\n")
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failing on purpose", http.StatusInternalServerError) })
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "tick %d\n", i)
			fl.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	mux.HandleFunc("/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected Upgrade: websocket", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(conn, brw) // echo raw bytes until the client closes
	})
	srv := &http.Server{Addr: net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), Handler: mux}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		fmt.Println("SIGTERM received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	fmt.Println("listening on", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------- upgrade

func upgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	addr := fs.String("addr", "", "dootd address, e.g. 198.18.0.10:443")
	host := fs.String("host", "", "SNI and Host")
	ca := fs.String("ca", "", "root to verify dootd's certificate")
	cert := fs.String("cert", "", "client certificate (AOP)")
	key := fs.String("key", "", "client key")
	fs.Parse(args)

	pool := x509.NewCertPool()
	b, err := os.ReadFile(*ca)
	if err != nil || !pool.AppendCertsFromPEM(b) {
		log.Fatalf("reading CA: %v", err)
	}
	cfg := &tls.Config{ServerName: *host, RootCAs: pool, NextProtos: []string{"http/1.1"}}
	if *cert != "" {
		c, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			log.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", *addr, cfg)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nCF-Connecting-IP: 203.0.113.7\r\n\r\n", *host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		log.Fatalf("reading response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		log.Fatalf("expected 101, got %s", resp.Status)
	}
	msg := "hello through dootd " + randID()
	fmt.Fprint(conn, msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(br, got); err != nil {
		log.Fatalf("reading echo: %v", err)
	}
	if string(got) != msg {
		log.Fatalf("echo mismatch: %q", got)
	}
	fmt.Println("OK upgrade echo")
}
