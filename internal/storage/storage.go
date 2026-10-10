// Package storage stores uploaded files and backups behind pluggable drivers. Drivers
// register themselves by name (like database/sql drivers); the built-in ones are "local"
// (a directory on disk) and "s3" (any S3-compatible object store: AWS S3, Cloudflare R2,
// MinIO, Backblaze B2, Google Cloud Storage through its XML API, …).
//
// Keys are slash-separated relative paths such as "avatars/0190….png". Drivers must
// reject keys that are empty, absolute, or contain "." / ".." segments (see ValidKey).
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned (possibly wrapped) when an object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Object describes a stored object.
type Object struct {
	Key         string
	Size        int64
	ContentType string
	ModTime     time.Time
}

// Backend is a storage driver instance.
type Backend interface {
	// Driver returns the registered driver name.
	Driver() string
	// Describe returns a short, credential-free description for logs and checks,
	// e.g. "local directory /data" or "s3 bucket gotalk at https://s3.amazonaws.com".
	Describe() string
	// Put stores size bytes from r under key, replacing any existing object.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens an object for reading. The caller closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
	// Stat returns an object's metadata.
	Stat(ctx context.Context, key string) (Object, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List calls fn for every object whose key starts with prefix, in lexical key order.
	// Returning an error from fn stops the listing and returns that error.
	List(ctx context.Context, prefix string, fn func(Object) error) error
}

// Settings configures the storage backend. Every field can be set in the config file, as
// a GOTALK_STORAGE_* environment variable, or through the setup wizard.
type Settings struct {
	// Driver selects the backend: "local" or "s3" (or a third-party driver).
	Driver string `koanf:"driver" json:"driver" doc:"Storage driver"`
	// LocalPath is the directory used by the local driver.
	LocalPath string `koanf:"local_path" json:"local_path,omitempty" doc:"Directory for the local driver"`
	// S3Endpoint is the S3 API endpoint, e.g. https://s3.us-east-1.amazonaws.com,
	// https://<account>.r2.cloudflarestorage.com or http://minio:9000. Empty means AWS S3
	// in S3Region.
	S3Endpoint string `koanf:"s3_endpoint" json:"s3_endpoint,omitempty" doc:"S3 API endpoint; empty means AWS S3 in s3_region"`
	S3Region   string `koanf:"s3_region" json:"s3_region,omitempty" doc:"Signing region (us-east-1 when unsure; auto for R2)"`
	S3Bucket   string `koanf:"s3_bucket" json:"s3_bucket,omitempty"`
	// S3Prefix is prepended to every key, so one bucket can hold several instances.
	S3Prefix          string `koanf:"s3_prefix" json:"s3_prefix,omitempty" doc:"Key prefix inside the bucket"`
	S3AccessKeyID     string `koanf:"s3_access_key_id" json:"s3_access_key_id,omitempty"`
	S3SecretAccessKey string `koanf:"s3_secret_access_key" json:"s3_secret_access_key,omitempty" doc:"Write-only; leave empty to keep the current value"`
	// S3ForcePathStyle addresses buckets as <endpoint>/<bucket> instead of
	// <bucket>.<endpoint>; MinIO and most self-hosted stores need it.
	S3ForcePathStyle bool `koanf:"s3_force_path_style" json:"s3_force_path_style,omitempty" doc:"Use path-style bucket addressing (MinIO, SeaweedFS and most self-hosted stores)"`
	// PublicURL, when set, is the base URL files are served from directly (a CDN or a
	// public bucket). Otherwise Gotalk serves them itself under /media/.
	PublicURL string `koanf:"public_url" json:"public_url,omitempty" doc:"Serve files from this base URL (CDN or public bucket) instead of through Gotalk"`
	// Options carries settings for third-party drivers.
	Options map[string]string `koanf:"options" json:"options,omitempty"`
}

// Redacted returns a copy with secrets removed, safe to show to administrators.
func (s Settings) Redacted() Settings {
	s.S3SecretAccessKey = ""
	return s
}

// SecretsSet lists the secret fields that hold a value.
func (s Settings) SecretsSet() []string {
	if s.S3SecretAccessKey != "" {
		return []string{"s3_secret_access_key"}
	}
	return []string{}
}

// KeepSecrets fills empty secret fields from prev, so callers can update settings
// without re-entering secrets.
func (s Settings) KeepSecrets(prev Settings) Settings {
	if s.S3SecretAccessKey == "" {
		s.S3SecretAccessKey = prev.S3SecretAccessKey
	}
	return s
}

// Factory opens a backend from settings. It should validate the settings but must not
// perform network calls; use Check for that.
type Factory func(Settings) (Backend, error)

var (
	mu      sync.RWMutex
	drivers = map[string]Factory{}
)

// Register makes a driver available by name. It panics if the name is taken.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := drivers[name]; dup {
		panic("storage: driver registered twice: " + name)
	}
	drivers[name] = f
}

// Drivers lists the registered driver names, sorted.
func Drivers() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Open builds a backend from settings.
func Open(s Settings) (Backend, error) {
	mu.RLock()
	f, ok := drivers[s.Driver]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown storage driver %q (available: %s)", s.Driver, strings.Join(Drivers(), ", "))
	}
	return f(s)
}

// ValidKey reports whether key is a safe relative object key.
func ValidKey(key string) bool {
	if key == "" || len(key) > 512 || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// CheckKey returns an error for keys ValidKey rejects. Drivers call it first.
func CheckKey(key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	return nil
}

// ProbePrefix holds the short-lived objects written by Check.
const ProbePrefix = ".gotalk-probe/"

// Check verifies that a backend is reachable and writable by writing, reading back and
// deleting a small probe object.
func Check(ctx context.Context, b Backend) error {
	key := fmt.Sprintf("%s%d", ProbePrefix, time.Now().UnixNano())
	payload := "gotalk storage check"
	if err := b.Put(ctx, key, strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		return fmt.Errorf("writing a test file: %w", err)
	}
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		_ = b.Delete(context.WithoutCancel(ctx), key)
		return fmt.Errorf("reading the test file back: %w", err)
	}
	got, err := io.ReadAll(io.LimitReader(rc, 1024))
	_ = rc.Close()
	if err == nil && string(got) != payload {
		err = errors.New("the test file came back with different contents")
	}
	if derr := b.Delete(ctx, key); derr != nil && err == nil {
		err = fmt.Errorf("deleting the test file: %w", derr)
	}
	return err
}
