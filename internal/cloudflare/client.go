// Package cloudflare is a minimal client for the Cloudflare v4 REST API:
// the calls dootd needs for DNS, Origin CA certificates, zone-level
// Authenticated Origin Pulls, the SSL mode and the published IP ranges.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBase is the production API.
const DefaultBase = "https://api.cloudflare.com/client/v4"

// Client talks to the API with an API token.
type Client struct {
	Token string
	Base  string
	HTTP  *http.Client
}

// Error is an API error with the permission hint for the failing call.
type Error struct {
	Op     string
	Status int
	Msgs   []string
	Hint   string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("cloudflare: %s: HTTP %d", e.Op, e.Status)
	if len(e.Msgs) > 0 {
		s += ": " + strings.Join(e.Msgs, "; ")
	}
	if e.Hint != "" && (e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized) {
		s += " (the API token probably lacks the " + e.Hint + " permission)"
	}
	return s
}

// IsNotFound reports a 404 from the API.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsForbidden reports a 401 or 403 from the API: the token lacks a permission.
func IsForbidden(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized)
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMsg        `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiMsg struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path, hint string, body, out any) error {
	base := c.Base
	if base == "" {
		base = DefaultBase
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	op := method + " " + strings.SplitN(path, "?", 2)[0]
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("cloudflare: %s: %w", op, err)
	}
	var env envelope
	jerr := json.Unmarshal(raw, &env)
	if resp.StatusCode >= 300 || jerr != nil || !env.Success {
		e := &Error{Op: op, Status: resp.StatusCode, Hint: hint}
		for _, m := range env.Errors {
			e.Msgs = append(e.Msgs, fmt.Sprintf("%s (code %d)", m.Message, m.Code))
		}
		if jerr != nil && resp.StatusCode < 300 {
			e.Msgs = append(e.Msgs, "invalid response: "+jerr.Error())
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare: %s: decode result: %w", op, err)
		}
	}
	return nil
}

// Verify checks the token is valid and active.
func (c *Client) Verify(ctx context.Context) error {
	var r struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/tokens/verify", "", nil, &r); err != nil {
		return err
	}
	if r.Status != "active" {
		return fmt.Errorf("cloudflare: token status is %q, not active", r.Status)
	}
	return nil
}

// Zone is a Cloudflare zone.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // active, pending (nameservers not switched yet), initializing, moved
}

// Zones lists zones the token can read.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var zs []Zone
	err := c.do(ctx, http.MethodGet, "/zones?per_page=50", "Zone: Read", nil, &zs)
	return zs, err
}

// ZoneFor finds the zone serving hostname (longest matching suffix).
func (c *Client) ZoneFor(ctx context.Context, hostname string) (Zone, error) {
	labels := strings.Split(strings.ToLower(hostname), ".")
	for i := 0; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zs []Zone
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), "Zone: Read", nil, &zs); err != nil {
			return Zone{}, err
		}
		for _, z := range zs {
			if z.Name == name {
				return z, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("no Cloudflare zone found for %s (is the domain on this Cloudflare account, and can the token read it?)", hostname)
}

// DNSRecord is one DNS record.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

// DNSRecords lists records named name in the zone.
func (c *Client) DNSRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	var rs []DNSRecord
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?per_page=100&name="+url.QueryEscape(name), "DNS: Edit", nil, &rs)
	return rs, err
}

// UpsertProxied makes sure exactly one proxied record of typ (A or AAAA)
// for name points at content. It refuses to touch a conflicting CNAME.
// It returns what it did ("created", "updated" or "unchanged").
func (c *Client) UpsertProxied(ctx context.Context, zoneID, name, typ, content string) (string, error) {
	rs, err := c.DNSRecords(ctx, zoneID, name)
	if err != nil {
		return "", err
	}
	want := DNSRecord{Type: typ, Name: name, Content: content, Proxied: true, TTL: 1}
	var same []DNSRecord
	for _, r := range rs {
		switch r.Type {
		case "CNAME":
			return "", fmt.Errorf("%s has a CNAME record; delete it in Cloudflare so dootd can create the %s record", name, typ)
		case typ:
			same = append(same, r)
		}
	}
	if len(same) > 1 {
		return "", fmt.Errorf("%s has %d %s records; keep only one (dootd will then manage it)", name, len(same), typ)
	}
	if len(same) == 0 {
		return "created", c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", "DNS: Edit", want, nil)
	}
	if same[0].Content == content && same[0].Proxied {
		return "unchanged", nil
	}
	return "updated", c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+same[0].ID, "DNS: Edit",
		map[string]any{"content": content, "proxied": true}, nil)
}

// OriginCert is a Cloudflare Origin CA certificate.
type OriginCert struct {
	ID          string `json:"id"`
	Certificate string `json:"certificate"`
	ExpiresOn   string `json:"expires_on"`
}

// CreateOriginCert signs csrPEM (ECDSA) for hostnames.
func (c *Client) CreateOriginCert(ctx context.Context, csrPEM string, hostnames []string, days int) (OriginCert, error) {
	var oc OriginCert
	err := c.do(ctx, http.MethodPost, "/certificates", "SSL and Certificates: Edit", map[string]any{
		"csr":                csrPEM,
		"hostnames":          hostnames,
		"request_type":       "origin-ecc",
		"requested_validity": days,
	}, &oc)
	return oc, err
}

// RevokeOriginCert revokes an Origin CA certificate.
func (c *Client) RevokeOriginCert(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/certificates/"+id, "SSL and Certificates: Edit", nil, nil)
}

// SSLMode returns the zone's SSL/TLS mode: off, flexible, full or strict.
func (c *Client) SSLMode(ctx context.Context, zoneID string) (string, error) {
	var r struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/settings/ssl", "Zone Settings: Read", nil, &r)
	return r.Value, err
}

// SetSSLMode changes the zone's SSL/TLS mode.
func (c *Client) SetSSLMode(ctx context.Context, zoneID, mode string) error {
	return c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/settings/ssl", "Zone Settings: Edit", map[string]string{"value": mode}, nil)
}

// AOPCert is a zone-level Authenticated Origin Pulls client certificate.
type AOPCert struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // initializing, pending_deployment, active, ...
	ExpiresOn string `json:"expires_on"`
}

// UploadAOPCert uploads the client certificate Cloudflare presents to the origin.
func (c *Client) UploadAOPCert(ctx context.Context, zoneID, certPEM, keyPEM string) (AOPCert, error) {
	var r AOPCert
	err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/origin_tls_client_auth", "SSL and Certificates: Edit",
		map[string]string{"certificate": certPEM, "private_key": keyPEM}, &r)
	return r, err
}

// AOPCert returns one uploaded client certificate.
func (c *Client) AOPCert(ctx context.Context, zoneID, id string) (AOPCert, error) {
	var r AOPCert
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/origin_tls_client_auth/"+id, "SSL and Certificates: Read", nil, &r)
	return r, err
}

// ListAOPCerts lists uploaded client certificates.
func (c *Client) ListAOPCerts(ctx context.Context, zoneID string) ([]AOPCert, error) {
	var r []AOPCert
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/origin_tls_client_auth", "SSL and Certificates: Read", nil, &r)
	return r, err
}

// DeleteAOPCert deletes an uploaded client certificate.
func (c *Client) DeleteAOPCert(ctx context.Context, zoneID, id string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/origin_tls_client_auth/"+id, "SSL and Certificates: Edit", nil, nil)
}

// AOPEnabled reports whether zone-level AOP is on.
func (c *Client) AOPEnabled(ctx context.Context, zoneID string) (bool, error) {
	var r struct {
		Enabled bool `json:"enabled"`
	}
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/origin_tls_client_auth/settings", "Zone Settings: Read", nil, &r)
	return r.Enabled, err
}

// SetAOPEnabled turns zone-level AOP on or off.
func (c *Client) SetAOPEnabled(ctx context.Context, zoneID string, on bool) error {
	return c.do(ctx, http.MethodPut, "/zones/"+zoneID+"/origin_tls_client_auth/settings", "Zone Settings: Edit",
		map[string]bool{"enabled": on}, nil)
}

// IPRanges are Cloudflare's published edge ranges.
type IPRanges struct {
	IPv4 []string `json:"ipv4_cidrs"`
	IPv6 []string `json:"ipv6_cidrs"`
}

// IPs fetches the current edge IP ranges (no token needed).
func (c *Client) IPs(ctx context.Context) (IPRanges, error) {
	var r IPRanges
	err := c.do(ctx, http.MethodGet, "/ips", "", nil, &r)
	return r, err
}

// DeleteDNSRecord deletes one DNS record.
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, id string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+id, "DNS: Edit", nil, nil)
}
