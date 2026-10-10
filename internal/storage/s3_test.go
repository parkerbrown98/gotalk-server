package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newS3ForTest(t *testing.T, endpoint string, forcePath bool) *s3Backend {
	t.Helper()
	b, err := Open(Settings{
		Driver:            "s3",
		S3Endpoint:        endpoint,
		S3Bucket:          "bucket",
		S3AccessKeyID:     "access",
		S3SecretAccessKey: "secret",
		S3Prefix:          "/tenant",
		S3ForcePathStyle:  forcePath,
	})
	require.NoError(t, err)
	s3, ok := b.(*s3Backend)
	require.True(t, ok)
	return s3
}

func TestS3OpenValidation(t *testing.T) {
	_, err := Open(Settings{Driver: "s3"})
	require.EqualError(t, err, "storage.s3_bucket is required for the s3 driver")
	_, err = Open(Settings{Driver: "s3", S3Bucket: "b"})
	require.EqualError(t, err, "storage.s3_access_key_id is required for the s3 driver")
	_, err = Open(Settings{Driver: "s3", S3Bucket: "b", S3AccessKeyID: "a"})
	require.EqualError(t, err, "storage.s3_secret_access_key is required for the s3 driver")
	_, err = Open(Settings{Driver: "s3", S3Bucket: "b", S3AccessKeyID: "a", S3SecretAccessKey: "s", S3Endpoint: "://bad"})
	require.ErrorContains(t, err, "parsing storage.s3_endpoint")
	_, err = Open(Settings{Driver: "s3", S3Bucket: "b", S3AccessKeyID: "a", S3SecretAccessKey: "s", S3Endpoint: "ftp://example.com"})
	require.EqualError(t, err, "storage.s3_endpoint must be an absolute http or https URL")
	_, err = Open(Settings{Driver: "s3", S3Bucket: "b", S3AccessKeyID: "a", S3SecretAccessKey: "s", S3Endpoint: "https://example.com/base"})
	require.EqualError(t, err, "storage.s3_endpoint must not include a path")
}

func TestS3PathStylePutSignsAndEscapes(t *testing.T) {
	var gotAuth, gotHash, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHash = r.Header.Get("X-Amz-Content-Sha256")
		gotPath = r.URL.EscapedPath()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotBody = string(body)
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, int64(4), r.ContentLength)
		require.Equal(t, "text/plain", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := newS3ForTest(t, srv.URL, true)
	err := b.Put(context.Background(), "avatars/a b+%.txt", strings.NewReader("body"), 4, "text/plain")
	require.NoError(t, err)
	require.Contains(t, gotAuth, "AWS4-HMAC-SHA256 Credential=access/")
	require.Equal(t, "UNSIGNED-PAYLOAD", gotHash)
	require.Equal(t, "/bucket/tenant/avatars/a%20b%2B%25.txt", gotPath)
	require.Equal(t, "body", gotBody)
}

func TestS3VirtualHostedURLConstruction(t *testing.T) {
	b := newS3ForTest(t, "https://s3.example.test:9443", false)
	u := b.objectURL("avatars/file name.txt")
	require.Equal(t, "bucket.s3.example.test:9443", u.Host)
	require.Equal(t, "/tenant/avatars/file name.txt", u.Path)
	require.Equal(t, "/tenant/avatars/file%20name.txt", u.EscapedPath())
	require.Equal(t, "s3 bucket bucket at https://s3.example.test:9443 prefix tenant/", b.Describe())
}

func TestS3GetStatDeleteAndNotFound(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		require.NotEmpty(t, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodGet:
			if strings.Contains(r.URL.Path, "missing") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", "3")
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			_, _ = w.Write([]byte("abc"))
		case http.MethodHead:
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "5")
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		case http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	b := newS3ForTest(t, srv.URL, true)
	rc, obj, err := b.Get(context.Background(), "avatars/a.png")
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "abc", string(body))
	require.Equal(t, int64(3), obj.Size)
	require.Equal(t, "image/png", obj.ContentType)
	require.False(t, obj.ModTime.IsZero())

	stat, err := b.Stat(context.Background(), "avatars/a.txt")
	require.NoError(t, err)
	require.Equal(t, int64(5), stat.Size)
	require.Equal(t, "text/plain", stat.ContentType)
	require.NoError(t, b.Delete(context.Background(), "avatars/a.txt"))

	_, _, err = b.Get(context.Background(), "avatars/missing.txt")
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, calls, "DELETE /bucket/tenant/avatars/a.txt")
}

func TestS3ListPaginationAndPrefixStripping(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/bucket", r.URL.EscapedPath())
		require.Equal(t, "2", r.URL.Query().Get("list-type"))
		require.Equal(t, "1000", r.URL.Query().Get("max-keys"))
		require.Equal(t, "tenant/avatars/0", r.URL.Query().Get("prefix"))
		require.NotEmpty(t, r.Header.Get("Authorization"))
		token := r.URL.Query().Get("continuation-token")
		tokens = append(tokens, token)
		w.Header().Set("Content-Type", "application/xml")
		if token == "" {
			_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>tenant/avatars/01.txt</Key><LastModified>2026-01-02T03:04:05Z</LastModified><Size>1</Size></Contents></ListBucketResult>`))
			return
		}
		require.Equal(t, "next", token)
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>tenant/avatars/02.bin</Key><LastModified>2026-01-02T03:04:06Z</LastModified><Size>2</Size></Contents></ListBucketResult>`))
	}))
	defer srv.Close()

	b := newS3ForTest(t, srv.URL, true)
	var got []Object
	err := b.List(context.Background(), "avatars/0", func(obj Object) error {
		got = append(got, obj)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"", "next"}, tokens)
	require.Len(t, got, 2)
	require.Equal(t, "avatars/01.txt", got[0].Key)
	require.Equal(t, int64(1), got[0].Size)
	require.Equal(t, "text/plain; charset=utf-8", got[0].ContentType)
	require.Equal(t, "avatars/02.bin", got[1].Key)
	require.Equal(t, "application/octet-stream", got[1].ContentType)
}

func TestS3ErrorXMLParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
	}))
	defer srv.Close()

	b := newS3ForTest(t, srv.URL, true)
	err := b.Put(context.Background(), "avatars/x.png", strings.NewReader("x"), 1, "image/png")
	require.EqualError(t, err, "s3: PUT avatars/x.png: AccessDenied: Access Denied (HTTP 403)")
	require.False(t, errors.Is(err, ErrNotFound))
}
