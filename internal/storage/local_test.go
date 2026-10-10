package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newLocalForTest(t *testing.T) (*localBackend, string) {
	t.Helper()
	root := t.TempDir()
	b, err := Open(Settings{Driver: "local", LocalPath: root})
	require.NoError(t, err)
	local, ok := b.(*localBackend)
	require.True(t, ok)
	return local, root
}

func TestLocalOpenRequiresPath(t *testing.T) {
	_, err := Open(Settings{Driver: "local"})
	require.EqualError(t, err, "storage.local_path is required for the local driver")
}

func TestLocalPutGetStatListDelete(t *testing.T) {
	ctx := context.Background()
	b, root := newLocalForTest(t)

	require.NoError(t, b.Put(ctx, "avatars/02.txt", strings.NewReader("two"), 3, "text/plain"))
	require.NoError(t, b.Put(ctx, "avatars/01.png", strings.NewReader("one"), 3, "image/png"))
	require.NoError(t, b.Put(ctx, "docs/readme", strings.NewReader("doc"), 3, "text/plain"))
	require.NoError(t, b.Put(ctx, "avatars/02.txt", strings.NewReader("TWO"), 3, "text/plain"))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gotalk-tmp-manual"), []byte("skip"), 0o600))

	rc, obj, err := b.Get(ctx, "avatars/01.png")
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "one", string(body))
	require.Equal(t, Object{
		Key:         "avatars/01.png",
		Size:        3,
		ContentType: "image/png",
		ModTime:     obj.ModTime,
	}, obj)
	require.False(t, obj.ModTime.IsZero())

	stat, err := b.Stat(ctx, "docs/readme")
	require.NoError(t, err)
	require.Equal(t, "application/octet-stream", stat.ContentType)
	require.Equal(t, int64(3), stat.Size)

	var keys []string
	err = b.List(ctx, "avatars/0", func(obj Object) error {
		keys = append(keys, obj.Key)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"avatars/01.png", "avatars/02.txt"}, keys)

	require.NoError(t, b.Delete(ctx, "avatars/01.png"))
	require.NoError(t, b.Delete(ctx, "avatars/01.png"))
	_, err = b.Stat(ctx, "avatars/01.png")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLocalNotFound(t *testing.T) {
	b, _ := newLocalForTest(t)
	_, _, err := b.Get(context.Background(), "missing.txt")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLocalMissingRootListsEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	b, err := Open(Settings{Driver: "local", LocalPath: root})
	require.NoError(t, err)
	calls := 0
	require.NoError(t, b.List(context.Background(), "", func(Object) error {
		calls++
		return nil
	}))
	require.Zero(t, calls)
}

func TestLocalRejectsEscapeAttempts(t *testing.T) {
	b, _ := newLocalForTest(t)
	for _, key := range []string{"../x", "/x", "a\\b", "a/../b"} {
		err := b.Put(context.Background(), key, strings.NewReader("x"), 1, "text/plain")
		require.Error(t, err)
	}
	require.Error(t, b.List(context.Background(), "../", func(Object) error { return nil }))
}

func TestLocalSizeMismatchRemovesTemp(t *testing.T) {
	b, root := newLocalForTest(t)
	err := b.Put(context.Background(), "bad.txt", strings.NewReader("abc"), 4, "text/plain")
	require.ErrorContains(t, err, "expected 4 bytes, wrote 3")
	_, statErr := os.Stat(filepath.Join(root, "bad.txt"))
	require.True(t, errors.Is(statErr, os.ErrNotExist))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
