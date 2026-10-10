package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidKey(t *testing.T) {
	tests := []struct {
		key string
		ok  bool
	}{
		{"avatars/0190.png", true},
		{".gotalk-probe/1", true},
		{"space name/a+b.txt", true},
		{"", false},
		{"/absolute", false},
		{"avatars//x", false},
		{"avatars/./x", false},
		{"avatars/../x", false},
		{"avatars\\x", false},
		{strings.Repeat("a", 513), false},
		{"avatars/\x7f", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			require.Equal(t, tt.ok, ValidKey(tt.key))
			err := CheckKey(tt.key)
			if tt.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRegistry(t *testing.T) {
	require.Contains(t, Drivers(), "local")
	require.Contains(t, Drivers(), "s3")

	_, err := Open(Settings{Driver: "missing"})
	require.ErrorContains(t, err, `unknown storage driver "missing"`)
}

func TestCheckAgainstLocal(t *testing.T) {
	b, err := Open(Settings{Driver: "local", LocalPath: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, Check(context.Background(), b))
}
