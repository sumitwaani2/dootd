package edge

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var hostnameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidHostname checks a lowercase DNS hostname (no wildcards, no port).
func ValidHostname(h string) error {
	if len(h) > 253 || !hostnameRe.MatchString(h) {
		return fmt.Errorf("invalid domain %q: use a lowercase hostname like app.example.com", h)
	}
	return nil
}

// CertStore holds one Origin CA certificate per hostname, on disk at
// <dir>/<hostname>/{cert.pem,key.pem} and in memory for SNI lookups.
type CertStore struct {
	dir   string
	mu    sync.RWMutex
	certs map[string]*tls.Certificate
}

// OpenCertStore loads every certificate under dir.
func OpenCertStore(dir string) (*CertStore, error) {
	s := &CertStore{dir: dir, certs: map[string]*tls.Certificate{}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || ValidHostname(e.Name()) != nil {
			continue
		}
		c, err := tls.LoadX509KeyPair(filepath.Join(dir, e.Name(), "cert.pem"), filepath.Join(dir, e.Name(), "key.pem"))
		if err != nil {
			continue // re-issued on the next sync
		}
		s.certs[e.Name()] = &c
	}
	return s, nil
}

// Get returns the certificate for hostname.
func (s *CertStore) Get(host string) *tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.certs[host]
}

// Leaf returns the parsed leaf certificate for hostname (nil if none).
func (s *CertStore) Leaf(host string) *x509.Certificate {
	if c := s.Get(host); c != nil {
		return c.Leaf
	}
	return nil
}

// NeedsCert reports whether hostname has no certificate that covers it for
// at least another `renewBefore`.
func (s *CertStore) NeedsCert(host string, renewBefore time.Duration) (bool, string) {
	l := s.Leaf(host)
	switch {
	case l == nil:
		return true, "no certificate yet"
	case l.VerifyHostname(host) != nil:
		return true, "certificate does not cover the hostname"
	case time.Until(l.NotAfter) < renewBefore:
		return true, fmt.Sprintf("certificate expires %s", l.NotAfter.Format(time.DateOnly))
	}
	return false, ""
}

// Put validates and stores a certificate (PEM chain) and key for hostname.
func (s *CertStore) Put(host string, certPEM, keyPEM []byte) (*x509.Certificate, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("edge: certificate for %s: %w", host, err)
	}
	if err := c.Leaf.VerifyHostname(host); err != nil {
		return nil, fmt.Errorf("edge: certificate for %s: %w", host, err)
	}
	d := filepath.Join(s.dir, host)
	if err := writeFileAtomic(filepath.Join(d, "key.pem"), keyPEM); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(d, "cert.pem"), certPEM); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[host] = &c
	s.mu.Unlock()
	return c.Leaf, nil
}

// Hosts lists hostnames with a certificate.
func (s *CertStore) Hosts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for h := range s.certs {
		out = append(out, h)
	}
	return out
}

// normalizeHost lowercases and strips a port and trailing dot.
func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}
