package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/parkerbrown98/gotalk-server/internal/sigv4"
)

func TestS3AgainstSeaweedFS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a SeaweedFS container")
	}
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		accessKey = "gotalk-access"
		secretKey = "gotalk-secret"
		bucket    = "gotalk-storage-test"
	)
	cfg := `{
  "identities": [{
    "name": "gotalk",
    "credentials": [{"accessKey": "` + accessKey + `", "secretKey": "` + secretKey + `"}],
    "actions": ["Admin", "Read", "Write", "List", "Tagging"]
  }]
}`
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "chrislusf/seaweedfs:latest",
			Cmd:          []string{"server", "-s3", "-s3.config=/etc/seaweedfs/s3.json", "-s3.port=8333", "-ip=0.0.0.0"},
			ExposedPorts: []string{"8333/tcp"},
			Files: []testcontainers.ContainerFile{{
				Reader:            strings.NewReader(cfg),
				ContainerFilePath: "/etc/seaweedfs/s3.json",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForListeningPort("8333/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = testcontainers.TerminateContainer(ctr)
	})

	endpoint, err := ctr.PortEndpoint(ctx, "8333/tcp", "http")
	require.NoError(t, err)
	require.NoError(t, createTestS3Bucket(ctx, endpoint, bucket, accessKey, secretKey))

	prefix := fmt.Sprintf("it-%d", time.Now().UnixNano())
	b, err := Open(Settings{
		Driver:            "s3",
		S3Endpoint:        endpoint,
		S3Region:          defaultS3Region,
		S3Bucket:          bucket,
		S3Prefix:          prefix,
		S3AccessKeyID:     accessKey,
		S3SecretAccessKey: secretKey,
		S3ForcePathStyle:  true,
	})
	require.NoError(t, err)

	require.NoError(t, b.Put(ctx, "avatars/01.txt", strings.NewReader("hello"), 5, "text/plain"))
	rc, obj, err := b.Get(ctx, "avatars/01.txt")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, "hello", string(got))
	require.Equal(t, int64(5), obj.Size)
	require.Equal(t, "text/plain", obj.ContentType)

	stat, err := b.Stat(ctx, "avatars/01.txt")
	require.NoError(t, err)
	require.Equal(t, int64(5), stat.Size)

	var listed []string
	require.NoError(t, b.List(ctx, "avatars/", func(obj Object) error {
		listed = append(listed, obj.Key)
		return nil
	}))
	require.Equal(t, []string{"avatars/01.txt"}, listed)
	require.NoError(t, Check(ctx, b))

	wrong, err := Open(Settings{
		Driver:            "s3",
		S3Endpoint:        endpoint,
		S3Region:          defaultS3Region,
		S3Bucket:          bucket,
		S3Prefix:          prefix,
		S3AccessKeyID:     accessKey,
		S3SecretAccessKey: "wrong-secret",
		S3ForcePathStyle:  true,
	})
	require.NoError(t, err)
	_, err = wrong.Stat(ctx, "avatars/01.txt")
	require.Error(t, err)

	require.NoError(t, b.Delete(ctx, "avatars/01.txt"))
	_, _, err = b.Get(ctx, "avatars/01.txt")
	require.ErrorIs(t, err, ErrNotFound)
}

func createTestS3Bucket(ctx context.Context, endpoint, bucket, accessKey, secretKey string) error {
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := putTestS3Bucket(ctx, endpoint, bucket, accessKey, secretKey); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func putTestS3Bucket(ctx context.Context, endpoint, bucket, accessKey, secretKey string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	u.Path, u.RawPath = escapedPath(bucket)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	signer := sigv4.Signer{
		Credentials: sigv4.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		Region:      defaultS3Region,
		Service:     "s3",
	}
	signer.Sign(req, sigv4.UnsignedPayload, time.Now())

	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusConflict {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if len(body) == 0 && res.StatusCode == http.StatusForbidden {
		return errors.New("creating test bucket: forbidden")
	}
	return fmt.Errorf("creating test bucket: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
}
