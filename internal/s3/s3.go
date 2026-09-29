// Package s3 is a thin wrapper over minio-go for any S3-compatible storage
// (Cloudflare R2, Backblaze B2, AWS S3, MinIO, ...).
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config describes the bucket. SecretKey is sensitive.
type Config struct {
	Endpoint  string `json:"endpoint"` // e.g. https://<account>.r2.cloudflarestorage.com
	Region    string `json:"region"`   // "auto" for R2
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// Normalize fills defaults and validates.
func (c *Config) Normalize() error {
	c.Endpoint = strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.Region = strings.TrimSpace(c.Region)
	c.AccessKey = strings.TrimSpace(c.AccessKey)
	if c.Region == "" {
		c.Region = "auto"
	}
	var errs []error
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		errs = append(errs, fmt.Errorf("endpoint %q must look like https://host (no path)", c.Endpoint))
	}
	if c.Bucket == "" {
		errs = append(errs, errors.New("bucket is required"))
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		errs = append(errs, errors.New("access key ID and secret access key are required"))
	}
	return errors.Join(errs...)
}

// Client talks to one bucket.
type Client struct {
	mc     *minio.Client
	bucket string
}

// Object is one listed object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// New creates a client (no network call).
func New(c Config) (*Client, error) {
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(c.Endpoint)
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       c.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	return &Client{mc: mc, bucket: c.Bucket}, nil
}

// PutFile uploads a local file (multipart for large files).
func (c *Client) PutFile(ctx context.Context, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = c.mc.PutObject(ctx, c.bucket, key, f, st.Size(), minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    16 << 20,
	})
	return wrap("upload "+key, err)
}

// GetFile downloads key into path.
func (c *Client) GetFile(ctx context.Context, key, path string) error {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return wrap("download "+key, err)
	}
	defer obj.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, obj); err != nil {
		f.Close()
		return wrap("download "+key, err)
	}
	return f.Close()
}

// List returns objects under prefix, sorted by key.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for o := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, wrap("list "+prefix, o.Err)
		}
		out = append(out, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Folders returns the top-level folder names in the bucket, sorted.
func (c *Client) Folders(ctx context.Context) ([]string, error) {
	var out []string
	for o := range c.mc.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{Recursive: false}) {
		if o.Err != nil {
			return nil, wrap("list folders", o.Err)
		}
		if name, ok := strings.CutSuffix(o.Key, "/"); ok && name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes one object.
func (c *Client) Delete(ctx context.Context, key string) error {
	return wrap("delete "+key, c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}))
}

// Test uploads, reads back and deletes a small object (Req 7.4).
func (c *Client) Test(ctx context.Context) error {
	key := ".dootd-connection-test"
	body := []byte("dootd connection test " + time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := c.mc.PutObject(ctx, c.bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		return wrap("test upload", err)
	}
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return wrap("test download", err)
	}
	got, err := io.ReadAll(obj)
	obj.Close()
	if err != nil {
		return wrap("test download", err)
	}
	if !bytes.Equal(got, body) {
		return errors.New("s3: test object read back differently than written")
	}
	return wrap("test delete", c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}))
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var r minio.ErrorResponse
	if errors.As(err, &r) && r.Code != "" {
		hint := ""
		switch r.Code {
		case "NoSuchBucket":
			hint = " (create the bucket first)"
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			hint = " (check the access key, secret and region; the key needs read, write and delete on the bucket)"
		}
		return fmt.Errorf("s3: %s: %s: %s%s", op, r.Code, r.Message, hint)
	}
	return fmt.Errorf("s3: %s: %w", op, err)
}
