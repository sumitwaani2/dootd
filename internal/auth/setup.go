package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/sumitwaani2/dootd/internal/store"
)

// One-time password (docs/architecture.md). The installer
// (`dootd setup-host`) creates it; signing in with it gives a session that
// can only set the admin email and password.
const (
	SettingSetupPassword = "setup_password"
	SetupValidity        = 24 * time.Hour
	setupAlphabet        = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i
	setupGroups          = 4
	setupGroupLen        = 5
)

// Errors of the one-time password flow.
var (
	ErrSetupInvalid  = errors.New("wrong or expired one-time password; run the install command again over SSH for a new one")
	ErrSetupRequired = errors.New("this session can only set up the admin account")
)

type setupRecord struct {
	Hash    string `json:"hash"`
	Expires int64  `json:"expires"`
	Used    bool   `json:"used"` // signed in once; stays until the account is set up
}

// NewSetupPassword creates a new one-time password, replacing any previous
// one, and returns it with its expiry. Only its argon2id hash is stored.
// dootd must not be running (the installer stops it first), because the
// service caches the state in memory.
func NewSetupPassword(ctx context.Context, db *store.Store) (string, time.Time, error) {
	var b strings.Builder
	for g := 0; g < setupGroups; g++ {
		if g > 0 {
			b.WriteByte('-')
		}
		for i := 0; i < setupGroupLen; i++ {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(setupAlphabet))))
			if err != nil {
				return "", time.Time{}, err
			}
			b.WriteByte(setupAlphabet[n.Int64()])
		}
	}
	pw := b.String()
	h, err := HashPassword(normalizeSetup(pw))
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(SetupValidity)
	v, _ := json.Marshal(setupRecord{Hash: h, Expires: exp.Unix()})
	if err := db.SetSetting(ctx, SettingSetupPassword, v); err != nil {
		return "", time.Time{}, err
	}
	return pw, exp, nil
}

// normalizeSetup ignores case, spaces and dashes in what was typed.
func normalizeSetup(pw string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(pw)))
}

// setupState caches the one-time password record.
type setupState struct {
	mu     sync.Mutex
	loaded bool
	rec    *setupRecord
}

func (a *Auth) setupRecord(ctx context.Context) *setupRecord {
	a.setup.mu.Lock()
	defer a.setup.mu.Unlock()
	if !a.setup.loaded {
		if v, ok, err := a.db.GetSetting(ctx, SettingSetupPassword); err == nil {
			a.setup.loaded = true
			if ok {
				var r setupRecord
				if json.Unmarshal(v, &r) == nil {
					a.setup.rec = &r
				}
			}
		}
	}
	if a.setup.rec == nil || time.Now().Unix() >= a.setup.rec.Expires {
		return nil
	}
	c := *a.setup.rec
	return &c
}

// SetupUntil is when the pending one-time password expires (zero if none is
// pending). A password that was used to sign in stays pending until the
// account is set up, so the setup address stays open for that session.
func (a *Auth) SetupUntil() time.Time {
	if r := a.setupRecord(context.Background()); r != nil {
		return time.Unix(r.Expires, 0)
	}
	return time.Time{}
}

// SetupUnused reports whether an unused one-time password exists.
func (a *Auth) SetupUnused() bool {
	r := a.setupRecord(context.Background())
	return r != nil && !r.Used
}

// LoginSetup signs in with the one-time password. It works once.
func (a *Auth) LoginSetup(ctx context.Context, password, ip, userAgent string) (string, error) {
	if err := a.allow(ip); err != nil {
		return "", err
	}
	r := a.setupRecord(ctx)
	if r == nil || r.Used {
		// Hash anyway so timing does not reveal whether one is pending.
		VerifyPassword(password, dummyHash())
		a.fail(ip)
		return "", ErrSetupInvalid
	}
	if !VerifyPassword(normalizeSetup(password), r.Hash) {
		a.fail(ip)
		return "", ErrSetupInvalid
	}
	r.Used = true
	if err := a.saveSetup(ctx, r); err != nil {
		return "", err
	}
	return a.newSession(ctx, ip, userAgent, true)
}

func (a *Auth) saveSetup(ctx context.Context, r *setupRecord) error {
	a.setup.mu.Lock()
	defer a.setup.mu.Unlock()
	if r == nil {
		if _, err := a.db.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingSetupPassword); err != nil {
			return err
		}
	} else {
		v, _ := json.Marshal(r)
		if err := a.db.SetSetting(ctx, SettingSetupPassword, v); err != nil {
			return err
		}
	}
	a.setup.rec, a.setup.loaded = r, true
	return nil
}

// CompleteSetup sets the admin email and password from a setup session,
// consumes the one-time password, revokes every session and returns a new,
// normal session token.
func (a *Auth) CompleteSetup(ctx context.Context, cur *Session, email, password, ip, userAgent string) (string, error) {
	if cur == nil || !cur.Setup {
		return "", ErrSetupRequired
	}
	if err := a.SetAdmin(ctx, email, password); err != nil {
		return "", err
	}
	if err := a.saveSetup(ctx, nil); err != nil {
		return "", err
	}
	return a.newSession(ctx, ip, userAgent, false)
}
