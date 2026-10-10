package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

const localTempPattern = ".gotalk-tmp-*"

func init() {
	Register("local", openLocal)
}

type localBackend struct {
	root string
}

func openLocal(s Settings) (Backend, error) {
	if s.LocalPath == "" {
		return nil, errors.New("storage.local_path is required for the local driver")
	}
	root, err := filepath.Abs(s.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("resolving storage.local_path: %w", err)
	}
	return &localBackend{root: filepath.Clean(root)}, nil
}

func (b *localBackend) Driver() string {
	return "local"
}

func (b *localBackend) Describe() string {
	return "local directory " + b.root
}

func (b *localBackend) Put(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	name, err := b.pathForKey(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating parent directory for %q: %w", key, err)
	}
	// #nosec G304 -- key is validated and resolved under the configured root.
	tmp, err := os.CreateTemp(dir, localTempPattern)
	if err != nil {
		return fmt.Errorf("creating temporary file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	written, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("writing %q: %w", key, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing temporary file for %q: %w", key, closeErr)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("writing %q: expected %d bytes, wrote %d", key, size, written)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("replacing %q: %w", key, err)
	}
	keepTemp = true
	return nil
}

func (b *localBackend) Get(_ context.Context, key string) (io.ReadCloser, Object, error) {
	if err := CheckKey(key); err != nil {
		return nil, Object{}, err
	}
	name, err := b.pathForKey(key)
	if err != nil {
		return nil, Object{}, err
	}
	// #nosec G304 -- key is validated and resolved under the configured root.
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Object{}, fmt.Errorf("local: get %q: %w", key, ErrNotFound)
		}
		return nil, Object{}, fmt.Errorf("opening %q: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil, Object{}, fmt.Errorf("local: stat %q: %w", key, ErrNotFound)
		}
		return nil, Object{}, fmt.Errorf("stat %q: %w", key, err)
	}
	return f, objectFromFileInfo(key, info), nil
}

func (b *localBackend) Stat(_ context.Context, key string) (Object, error) {
	if err := CheckKey(key); err != nil {
		return Object{}, err
	}
	name, err := b.pathForKey(key)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Object{}, fmt.Errorf("local: stat %q: %w", key, ErrNotFound)
		}
		return Object{}, fmt.Errorf("stat %q: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		return Object{}, fmt.Errorf("local: stat %q: %w", key, ErrNotFound)
	}
	return objectFromFileInfo(key, info), nil
}

func (b *localBackend) Delete(_ context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	name, err := b.pathForKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting %q: %w", key, err)
	}
	return nil
}

func (b *localBackend) List(ctx context.Context, prefix string, fn func(Object) error) error {
	if err := checkListPrefix(prefix); err != nil {
		return err
	}
	if _, err := os.Stat(b.root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat local root: %w", err)
	}
	return filepath.WalkDir(b.root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), strings.TrimSuffix(localTempPattern, "*")) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(b.root, name)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		return fn(objectFromFileInfo(key, info))
	})
}

func (b *localBackend) pathForKey(key string) (string, error) {
	name := filepath.Join(b.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(b.root, name)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", key, err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("storage: key %q escapes local root", key)
	}
	return name, nil
}

func objectFromFileInfo(key string, info os.FileInfo) Object {
	return Object{
		Key:         key,
		Size:        info.Size(),
		ContentType: contentTypeForKey(key),
		ModTime:     info.ModTime(),
	}
}

func contentTypeForKey(key string) string {
	if typ := mime.TypeByExtension(filepath.Ext(key)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func checkListPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") || len(prefix) > 512 {
		return fmt.Errorf("storage: invalid key prefix %q", prefix)
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("storage: invalid key prefix %q", prefix)
		}
	}
	for _, r := range prefix {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("storage: invalid key prefix %q", prefix)
		}
	}
	return nil
}
