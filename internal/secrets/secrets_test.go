package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newKey(t *testing.T) (*Box, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "master.key")
	b, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	return b, p
}

func TestCreateAndReload(t *testing.T) {
	b, p := newKey(t)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	if len(raw) != 65 || raw[64] != '\n' {
		t.Fatalf("key file should be 64 hex chars + newline, got %q", raw)
	}
	ct, err := b.Seal([]byte("hello"), "p")
	if err != nil {
		t.Fatal(err)
	}
	// Loading again must return the same key, never a new one.
	b2, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := b2.Open(ct, "p"); err != nil || string(pt) != "hello" {
		t.Fatalf("reload: %q %v", pt, err)
	}
	if raw2, _ := os.ReadFile(p); !bytes.Equal(raw, raw2) {
		t.Fatal("existing key was overwritten")
	}
}

func TestSealOpen(t *testing.T) {
	b, _ := newKey(t)
	ct, err := b.Seal([]byte("secret"), "settings:github_token")
	if err != nil {
		t.Fatal(err)
	}
	if ct[0] != formatVersion || len(ct) != 1+nonceSize+len("secret")+16 {
		t.Fatalf("unexpected ciphertext layout (%d bytes)", len(ct))
	}
	ct2, _ := b.Seal([]byte("secret"), "settings:github_token")
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonce must be random")
	}
	if _, err := b.Open(ct, "settings:cloudflare_token"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong purpose: %v", err)
	}
	bad := append([]byte{}, ct...)
	bad[len(bad)-1] ^= 1
	if _, err := b.Open(bad, "settings:github_token"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := b.Open(ct[:5], "settings:github_token"); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
	if _, err := b.Seal([]byte("x"), ""); !errors.Is(err, ErrEmptyPurpose) {
		t.Fatalf("empty purpose: %v", err)
	}
	other, _ := newKey(t)
	if _, err := other.Open(ct, "settings:github_token"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("other key: %v", err)
	}
}

func TestLoadRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), mode)
		os.Chmod(p, mode)
		return p
	}
	good := strings.Repeat("ab", 32) + "\n"
	if _, err := Load(write("perm", good, 0o640)); !errors.Is(err, ErrInsecurePermissions) {
		t.Errorf("group-readable: %v", err)
	}
	if _, err := Load(write("short", "abcd\n", 0o600)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("short: %v", err)
	}
	if _, err := Load(write("long", good+"00", 0o600)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("long: %v", err)
	}
	if _, err := Load(write("nothex", strings.Repeat("zz", 32), 0o600)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("not hex: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	p := write("bad-existing", "junk", 0o600)
	if _, err := LoadOrCreate(p); err == nil {
		t.Error("LoadOrCreate must not replace an invalid existing key")
	}
	if b, _ := os.ReadFile(p); string(b) != "junk" {
		t.Error("existing file was modified")
	}
}

func TestInstall(t *testing.T) {
	b, p := newKey(t)
	raw, _ := os.ReadFile(p)
	ct, _ := b.Seal([]byte("v"), "p")

	dst := filepath.Join(t.TempDir(), "master.key")
	b2, err := Install(dst, strings.ToUpper(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := b2.Open(ct, "p"); err != nil || string(pt) != "v" {
		t.Fatalf("installed key does not open: %v", err)
	}
	if _, err := Install(dst, string(raw)); err != nil {
		t.Fatalf("same key again: %v", err)
	}
	if _, err := Install(dst, strings.Repeat("11", 32)); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("different key: %v", err)
	}
	if _, err := Install(filepath.Join(t.TempDir(), "k"), "nope"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid: %v", err)
	}
}
