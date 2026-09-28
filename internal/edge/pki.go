package edge

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// writeFileAtomic writes data with mode 0600 via a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pemEncode(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func keyPEM(k crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pemEncode("PRIVATE KEY", der), nil
}

// newCSR makes a P-256 key and a CSR for hostname.
func newCSR(hostname string) (csrPEM, keyPEMBytes []byte, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}, k)
	if err != nil {
		return nil, nil, err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE REQUEST", der), kp, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// AOP is dootd's private CA for Authenticated Origin Pulls and the client
// certificate (signed by it) that Cloudflare presents to the origin.
type AOP struct {
	Dir       string
	CA        *x509.Certificate
	Pool      *x509.CertPool
	ClientPEM []byte
	ClientKey []byte
	Client    *x509.Certificate
}

const (
	aopCAValidity     = 10 * 365 * 24 * time.Hour
	aopClientValidity = 5 * 365 * 24 * time.Hour
	aopRenewBefore    = 60 * 24 * time.Hour
)

// LoadOrCreateAOP loads dir/{ca,client}.{pem,key}, creating the CA and/or
// the client certificate when missing, and rotating the client certificate
// when it expires within 60 days. rotated reports a new client certificate.
func LoadOrCreateAOP(dir string) (a *AOP, rotated bool, err error) {
	a = &AOP{Dir: dir}
	caCert, caKey, err := loadPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if errors.Is(err, os.ErrNotExist) {
		caCert, caKey, err = createCA(dir)
	}
	if err != nil {
		return nil, false, fmt.Errorf("edge: AOP CA: %w", err)
	}
	a.CA = caCert
	a.Pool = x509.NewCertPool()
	a.Pool.AddCert(caCert)

	cp, kp := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	cl, _, err := loadPair(cp, kp)
	if err == nil && time.Until(cl.NotAfter) < aopRenewBefore {
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := createClient(dir, caCert, caKey); err != nil {
			return nil, false, fmt.Errorf("edge: AOP client certificate: %w", err)
		}
		rotated = true
		cl, _, err = loadPair(cp, kp)
	}
	if err != nil {
		return nil, false, fmt.Errorf("edge: AOP client certificate: %w", err)
	}
	a.Client = cl
	if a.ClientPEM, err = os.ReadFile(cp); err != nil {
		return nil, false, err
	}
	if a.ClientKey, err = os.ReadFile(kp); err != nil {
		return nil, false, err
	}
	return a, rotated, nil
}

func loadPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		if _, serr := os.Stat(certPath); errors.Is(serr, os.ErrNotExist) {
			return nil, nil, os.ErrNotExist
		}
		return nil, nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("unsupported key type")
	}
	return pair.Leaf, signer, nil
}

// RSA is used because it is what Cloudflare documents for AOP uploads.
func createCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "dootd Authenticated Origin Pulls CA", Organization: []string{"dootd"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(aopCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return nil, nil, err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "ca.key"), kp); err != nil {
		return nil, nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "ca.pem"), pemEncode("CERTIFICATE", der)); err != nil {
		return nil, nil, err
	}
	c, err := x509.ParseCertificate(der)
	return c, k, err
}

func createClient(dir string, ca *x509.Certificate, caKey crypto.Signer) error {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "dootd origin pull client", Organization: []string{"dootd"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(aopClientValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "client.key"), kp); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "client.pem"), pemEncode("CERTIFICATE", der))
}
