package auth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sumitwaani2/dootd/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "dootd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSetupFlow(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	pw, until, err := NewSetupPassword(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z2-9]{5}(-[a-z2-9]{5}){3}$`).MatchString(pw) {
		t.Fatalf("format: %q", pw)
	}
	if d := time.Until(until); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expiry %v", d)
	}
	if v, _, _ := st.GetSetting(ctx, SettingSetupPassword); strings.Contains(string(v), pw) {
		t.Fatal("the plain password must not be stored")
	}

	a := New(st)
	if a.SetupUntil().IsZero() || !a.SetupUnused() {
		t.Fatal("pending password not seen")
	}
	if _, err := a.LoginSetup(ctx, "wrong-wrong-wrong-wrong", "1.1.1.1", "ua"); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("wrong password: %v", err)
	}
	// Case, spaces and dashes don't matter.
	tok, err := a.LoginSetup(ctx, " "+strings.ToUpper(strings.ReplaceAll(pw, "-", " "))+" ", "1.1.1.2", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoginSetup(ctx, pw, "1.1.1.3", "ua"); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("second use must fail: %v", err)
	}
	if a.SetupUnused() || a.SetupUntil().IsZero() {
		t.Fatal("a used password stays pending until the account is set up")
	}
	sess, err := a.Session(ctx, tok)
	if err != nil || !sess.Setup {
		t.Fatalf("setup session: %v %+v", err, sess)
	}

	// A normal session cannot complete setup.
	if _, err := a.CompleteSetup(ctx, &Session{}, "a@example.com", "long-enough-password", "", ""); !errors.Is(err, ErrSetupRequired) {
		t.Fatalf("normal session: %v", err)
	}
	if _, err := a.CompleteSetup(ctx, sess, "a@example.com", "short", "", ""); err == nil {
		t.Fatal("short password accepted")
	}
	tok2, err := a.CompleteSetup(ctx, sess, "Admin@Example.com", "long-enough-password", "1.1.1.2", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if !a.SetupUntil().IsZero() {
		t.Fatal("setup still pending after completing it")
	}
	if _, err := a.Session(ctx, tok); err == nil {
		t.Fatal("the setup session must be revoked")
	}
	if s2, err := a.Session(ctx, tok2); err != nil || s2.Setup {
		t.Fatalf("new session: %v %+v", err, s2)
	}
	if email, _ := a.Admin(ctx); email != "admin@example.com" {
		t.Fatalf("admin %q", email)
	}
	if _, err := a.Login(ctx, "admin@example.com", "long-enough-password", "1.1.1.4", "ua"); err != nil {
		t.Fatal(err)
	}

	// Email change needs the current password.
	if err := a.ChangeEmail(ctx, "nope", "b@example.com", "1.1.1.5"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := a.ChangeEmail(ctx, "long-enough-password", "B@example.com", "1.1.1.5"); err != nil {
		t.Fatal(err)
	}
	if email, _ := a.Admin(ctx); email != "b@example.com" {
		t.Fatalf("admin %q", email)
	}
}

func TestSetupExpires(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	pw, _, err := NewSetupPassword(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := st.GetSetting(ctx, SettingSetupPassword)
	var rec setupRecord
	json.Unmarshal(v, &rec)
	rec.Expires = time.Now().Add(-time.Second).Unix()
	v, _ = json.Marshal(rec)
	st.SetSetting(ctx, SettingSetupPassword, v)

	a := New(st)
	if !a.SetupUntil().IsZero() {
		t.Fatal("expired password still pending")
	}
	if _, err := a.LoginSetup(ctx, pw, "1.1.1.1", "ua"); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("expired password accepted: %v", err)
	}
}
