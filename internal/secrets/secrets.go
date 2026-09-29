// Package secrets manages dootd's master key and encrypts values at rest
// with AES-256-GCM.
//
// Key file: 64 lowercase hex characters (32 bytes) followed by a newline,
// mode 0600, owned by root. It is never overwritten once created.
//
// Ciphertext format: version(1) || nonce(12) || GCM ciphertext+tag.
// The caller-supplied purpose string is bound as GCM additional data, so a
// ciphertext copied into a different field fails to decrypt.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	keySize       = 32
	formatVersion = 0x01
	nonceSize     = 12
)

// Errors returned by this package.
var (
	ErrInsecurePermissions = errors.New("secrets: master key file must not be accessible by group or others (expected mode 0600)")
	ErrInvalidKey          = errors.New("secrets: master key file must contain exactly 64 hex characters")
	ErrDecrypt             = errors.New("secrets: decryption failed (wrong key, wrong purpose, or corrupted data)")
	ErrEmptyPurpose        = errors.New("secrets: purpose must not be empty")
)

// Box seals and opens secrets with the master key.
type Box struct {
	aead cipher.AEAD
}

// LoadOrCreate loads the master key at path, creating a new random key if the
// file does not exist. An existing file is never overwritten.
func LoadOrCreate(path string) (*Box, error) {
	b, err := Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	if err := create(path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Lost a race with another creator; use the key that now exists.
			return Load(path)
		}
		return nil, err
	}
	return Load(path)
}

// Load loads an existing master key. It never creates one.
func Load(path string) (*Box, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open master key: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secrets: stat master key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets: master key %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o", ErrInsecurePermissions, path, info.Mode().Perm())
	}

	// Read one byte more than a valid file so over-long files are detected.
	buf, err := io.ReadAll(io.LimitReader(f, 2*keySize+2))
	if err != nil {
		return nil, fmt.Errorf("secrets: read master key: %w", err)
	}
	text := strings.TrimSuffix(string(buf), "\n")
	if len(text) != 2*keySize {
		return nil, ErrInvalidKey
	}
	key, err := hex.DecodeString(text)
	if err != nil {
		return nil, ErrInvalidKey
	}
	return newBox(key)
}

func create(path string) error {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("secrets: generate key: %w", err)
	}
	return write(path, key)
}

// ErrKeyMismatch means a different master key is already installed.
var ErrKeyMismatch = errors.New("secrets: a different master key already exists")

// Install writes a known key (64 hex characters, e.g. from a recovery kit)
// to path. If the file already holds the same key it succeeds; a different
// key is never overwritten (ErrKeyMismatch).
func Install(path, hexKey string) (*Box, error) {
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != keySize {
		return nil, ErrInvalidKey
	}
	if cur, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(cur)) != hexKey {
			return nil, fmt.Errorf("%w at %s", ErrKeyMismatch, path)
		}
		return Load(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("secrets: read master key: %w", err)
	}
	if err := write(path, key); err != nil {
		return nil, err
	}
	return Load(path)
}

func write(path string, key []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("secrets: create master key: %w", err)
	}
	// Enforce 0600 regardless of umask.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("secrets: chmod master key: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("secrets: write master key: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("secrets: sync master key: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets: close master key: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("secrets: open key dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("secrets: sync key dir: %w", err)
	}
	return nil
}

func newBox(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext bound to purpose (e.g. "settings:github_pat").
func (b *Box) Seal(plaintext []byte, purpose string) ([]byte, error) {
	if purpose == "" {
		return nil, ErrEmptyPurpose
	}
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+b.aead.Overhead())
	out[0] = formatVersion
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	return b.aead.Seal(out, out[1:1+nonceSize], plaintext, []byte(purpose)), nil
}

// Open decrypts a value produced by Seal with the same purpose.
func (b *Box) Open(ciphertext []byte, purpose string) ([]byte, error) {
	if purpose == "" {
		return nil, ErrEmptyPurpose
	}
	if len(ciphertext) < 1+nonceSize+b.aead.Overhead() || ciphertext[0] != formatVersion {
		return nil, ErrDecrypt
	}
	pt, err := b.aead.Open(nil, ciphertext[1:1+nonceSize], ciphertext[1+nonceSize:], []byte(purpose))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
