package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/store"
)

// Session lifetimes (Req 5.4).
const (
	IdleTimeout     = 7 * 24 * time.Hour
	AbsoluteTimeout = 30 * 24 * time.Hour
	touchEvery      = 5 * time.Minute
)

// Rate limits (Req 5.5).
const (
	failWindow    = 15 * time.Minute
	failsPerIP    = 5
	failsOverall  = 50
	overallMinGap = 2 * time.Second
)

// Errors.
var (
	ErrInvalidLogin = errors.New("wrong email or password")
	ErrRateLimited  = errors.New("too many failed sign-in attempts; try again in 15 minutes")
	ErrNoSession    = errors.New("not signed in")
	ErrNoAdmin      = errors.New("no admin account yet: sign in with the one-time password printed by the install command (leave the email empty)")
)

// Session is an authenticated dashboard session.
type Session struct {
	idHash    []byte
	CSRF      string
	CreatedAt time.Time
	LastSeen  time.Time
	IP        string
	UserAgent string
	Current   bool
	Setup     bool // signed in with the one-time password: may only set up the account
}

// ID is a short, non-secret identifier for display and revocation.
func (s Session) ID() string { return base64.RawURLEncoding.EncodeToString(s.idHash[:6]) }

// Auth manages the admin user and sessions.
type Auth struct {
	db *store.Store

	mu       sync.Mutex
	failures map[string][]time.Time
	overall  []time.Time
	lastFail time.Time

	setup setupState
}

// New returns an Auth backed by db.
func New(db *store.Store) *Auth {
	return &Auth{db: db, failures: map[string][]time.Time{}}
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

func randToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// NormalizeEmail validates and lowercases an email address.
func NormalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e {
		return "", fmt.Errorf("invalid email address %q", e)
	}
	return e, nil
}

// Admin returns the admin email, or ErrNoAdmin.
func (a *Auth) Admin(ctx context.Context) (string, error) {
	var email string
	err := a.db.Reader().QueryRowContext(ctx, `SELECT email FROM users WHERE id = 1`).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAdmin
	}
	return email, err
}

// SetAdmin creates or replaces the admin credentials and revokes every
// session (Req 2.6).
func (a *Auth) SetAdmin(ctx context.Context, email, password string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := a.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, password_hash, updated_at) VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET email = excluded.email, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		email, h, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// Login checks credentials and returns a new session token.
func (a *Auth) Login(ctx context.Context, email, password, ip, userAgent string) (string, error) {
	if err := a.allow(ip); err != nil {
		return "", err
	}
	var stored, hash string
	err := a.db.Reader().QueryRowContext(ctx, `SELECT email, password_hash FROM users WHERE id = 1`).Scan(&stored, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAdmin
	}
	if err != nil {
		return "", err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	emailOK := subtle.ConstantTimeCompare([]byte(email), []byte(stored)) == 1
	if !emailOK {
		hash = dummyHash()
	}
	if !VerifyPassword(password, hash) || !emailOK {
		a.fail(ip)
		return "", ErrInvalidLogin
	}
	a.mu.Lock()
	delete(a.failures, ip)
	a.mu.Unlock()
	return a.newSession(ctx, ip, userAgent, false)
}

func (a *Auth) newSession(ctx context.Context, ip, userAgent string, setup bool) (string, error) {
	tok := randToken()
	now := time.Now().Unix()
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	flag := 0
	if setup {
		flag = 1
	}
	_, err := a.db.Writer().ExecContext(ctx, `INSERT INTO sessions (id_hash, csrf, created_at, last_seen, ip, user_agent, setup)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, hashToken(tok), randToken(), now, now, ip, userAgent, flag)
	if err != nil {
		return "", err
	}
	a.prune(ctx)
	return tok, nil
}

func (a *Auth) prune(ctx context.Context) {
	now := time.Now()
	a.db.Writer().ExecContext(ctx, `DELETE FROM sessions WHERE last_seen < ? OR created_at < ?`,
		now.Add(-IdleTimeout).Unix(), now.Add(-AbsoluteTimeout).Unix())
}

// Session validates a session token.
func (a *Auth) Session(ctx context.Context, tok string) (*Session, error) {
	if tok == "" || len(tok) > 100 {
		return nil, ErrNoSession
	}
	s := &Session{idHash: hashToken(tok), Current: true}
	var created, seen, setup int64
	err := a.db.Reader().QueryRowContext(ctx, `SELECT csrf, created_at, last_seen, ip, user_agent, setup FROM sessions WHERE id_hash = ?`,
		s.idHash).Scan(&s.CSRF, &created, &seen, &s.IP, &s.UserAgent, &setup)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
	now := time.Now()
	s.Setup = setup == 1
	// A setup session ends with its one-time password.
	if now.Sub(s.LastSeen) > IdleTimeout || now.Sub(s.CreatedAt) > AbsoluteTimeout || (s.Setup && a.SetupUntil().IsZero()) {
		a.db.Writer().ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, s.idHash)
		return nil, ErrNoSession
	}
	if now.Sub(s.LastSeen) > touchEvery {
		a.db.Writer().ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE id_hash = ?`, now.Unix(), s.idHash)
	}
	return s, nil
}

// CheckCSRF compares a submitted token with the session's.
func (s *Session) CheckCSRF(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.CSRF)) == 1
}

// Logout deletes the session.
func (a *Auth) Logout(ctx context.Context, s *Session) error {
	_, err := a.db.Writer().ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, s.idHash)
	return err
}

// Sessions lists active sessions, newest first.
func (a *Auth) Sessions(ctx context.Context, cur *Session) ([]Session, error) {
	rows, err := a.db.Reader().QueryContext(ctx, `SELECT id_hash, created_at, last_seen, ip, user_agent FROM sessions ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var c, l int64
		if err := rows.Scan(&s.idHash, &c, &l, &s.IP, &s.UserAgent); err != nil {
			return nil, err
		}
		s.CreatedAt, s.LastSeen = time.Unix(c, 0), time.Unix(l, 0)
		s.Current = cur != nil && subtle.ConstantTimeCompare(s.idHash, cur.idHash) == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeOthers deletes every session except cur.
func (a *Auth) RevokeOthers(ctx context.Context, cur *Session) (int64, error) {
	res, err := a.db.Writer().ExecContext(ctx, `DELETE FROM sessions WHERE id_hash != ?`, cur.idHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ChangePassword verifies the current password, sets the new one and
// revokes all other sessions.
func (a *Auth) ChangePassword(ctx context.Context, cur *Session, oldPw, newPw, ip string) error {
	if err := a.allow(ip); err != nil {
		return err
	}
	var hash string
	if err := a.db.Reader().QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = 1`).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(oldPw, hash) {
		a.fail(ip)
		return errors.New("current password is wrong")
	}
	h, err := HashPassword(newPw)
	if err != nil {
		return err
	}
	if _, err := a.db.Writer().ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = 1`, h, time.Now().Unix()); err != nil {
		return err
	}
	_, err = a.RevokeOthers(ctx, cur)
	return err
}

// ChangeEmail verifies the current password and sets a new admin email.
func (a *Auth) ChangeEmail(ctx context.Context, password, email, ip string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	if err := a.allow(ip); err != nil {
		return err
	}
	var hash string
	if err := a.db.Reader().QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = 1`).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(password, hash) {
		a.fail(ip)
		return errors.New("current password is wrong")
	}
	_, err = a.db.Writer().ExecContext(ctx, `UPDATE users SET email = ?, updated_at = ? WHERE id = 1`, email, time.Now().Unix())
	return err
}

func (a *Auth) allow(ip string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.failures[ip] = recent(a.failures[ip], now)
	a.overall = recent(a.overall, now)
	if len(a.failures[ip]) >= failsPerIP {
		return ErrRateLimited
	}
	// Under a distributed attack, slow every login down instead of locking
	// the owner out completely.
	if len(a.overall) >= failsOverall && now.Sub(a.lastFail) < overallMinGap {
		return ErrRateLimited
	}
	return nil
}

func (a *Auth) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.failures[ip] = append(a.failures[ip], now)
	a.overall = append(a.overall, now)
	a.lastFail = now
	if len(a.failures) > 10000 { // bound memory
		for k, v := range a.failures {
			if len(recent(v, now)) == 0 {
				delete(a.failures, k)
			}
		}
	}
}

func recent(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > failWindow {
		i++
	}
	return ts[i:]
}
