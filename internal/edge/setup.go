package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// Setup address (docs/architecture.md): while setup is open, a
// connection from outside Cloudflare is let through the IP filter as a
// setupConn. It gets a self-signed certificate, no client-certificate
// requirement, and only ever reaches the dashboard.

// setupConn marks a connection accepted for the setup address.
type setupConn struct{ net.Conn }

type ctxKey int

const directKey ctxKey = 0

// connContext marks requests on setup connections (http.Server.ConnContext).
func connContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if _, ok := c.(*setupConn); ok {
		return context.WithValue(ctx, directKey, true)
	}
	return ctx
}

// IsDirect reports whether r arrived on the setup address rather than
// through Cloudflare.
func IsDirect(r *http.Request) bool {
	v, _ := r.Context().Value(directKey).(bool)
	return v
}

// loadOrCreateSetupCert returns the self-signed certificate of the setup
// address, creating it (ECDSA P-256, 10 years) on first use.
func loadOrCreateSetupCert(dir string) (*tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return &c, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("edge: setup certificate: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "dootd setup"},
		DNSNames:              []string{"dootd-setup"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
