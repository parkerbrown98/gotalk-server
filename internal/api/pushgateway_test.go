//go:build pushgateway

// The push gateway integration test runs the real gotalk-gateway container and checks that
// a notification on a test instance becomes a push the gateway accepts. It needs Docker and
// the gateway image (GOTALK_GATEWAY_IMAGE, default ghcr.io/parkerbrown98/gotalk-gateway:edge;
// CI builds it from the gateway's main branch):
//
//	go test -tags pushgateway -run TestRealPushGateway ./internal/api
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const gatewayConfig = `gateway:
  name: Integration Gateway
apps:
  - id: gotalk
    name: Gotalk
    ios:
      bundle_id: app.example.gotalk
      apns:
        team_id: TEAM123456
        key_id: KEY1234567
        private_key_file: /etc/gotalk-gateway/apns.p8
        # Never contacted: this gateway only queues (delivery.workers is 0).
        production_url: http://127.0.0.1:9
        sandbox_url: http://127.0.0.1:9
`

// startGateway runs the gateway image as an API-only replica with its own database on the
// shared Postgres container. Both containers reach the test process through
// host.docker.internal, and the gateway is in development mode so it accepts the test
// instance's http origin on a private network.
func startGateway(t *testing.T, instancePort int) string {
	t.Helper()
	image := os.Getenv("GOTALK_GATEWAY_IMAGE")
	if image == "" {
		image = "ghcr.io/parkerbrown98/gotalk-gateway:edge"
	}
	dbURL, err := url.Parse(freshDatabase(t))
	require.NoError(t, err)
	dbURL.Host = "host.docker.internal:" + dbURL.Port()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	ctx := context.Background()
	c, err := testcontainers.Run(ctx, image,
		testcontainers.WithExposedPorts("8090/tcp"),
		testcontainers.WithEnv(map[string]string{
			"GOTALK_GATEWAY_CONFIG":                           "/etc/gotalk-gateway/config.yaml",
			"GOTALK_GATEWAY_DATABASE_URL":                     dbURL.String(),
			"GOTALK_GATEWAY_INSTANCES_ALLOW_HTTP":             "true",
			"GOTALK_GATEWAY_INSTANCES_ALLOW_PRIVATE_NETWORKS": "true",
			"GOTALK_GATEWAY_INSTANCES_ALLOWED_PORTS":          strconv.Itoa(instancePort),
			"GOTALK_GATEWAY_DELIVERY_WORKERS":                 "0",
			"GOTALK_GATEWAY_RATELIMIT_ENABLED":                "false",
			"GOTALK_GATEWAY_LOG_FORMAT":                       "text",
		}),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{Reader: strings.NewReader(gatewayConfig), ContainerFilePath: "/etc/gotalk-gateway/config.yaml", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(p8), ContainerFilePath: "/etc/gotalk-gateway/apns.p8", FileMode: 0o644},
		),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/readyz").WithPort("8090/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if c != nil {
		t.Cleanup(func() {
			if t.Failed() {
				if logs, err := c.Logs(ctx); err == nil {
					var b bytes.Buffer
					_, _ = b.ReadFrom(logs)
					t.Logf("gateway logs:\n%s", b.String())
				}
			}
			_ = testcontainers.TerminateContainer(c)
		})
	}
	require.NoError(t, err)
	endpoint, err := c.PortEndpoint(ctx, "8090/tcp", "http")
	require.NoError(t, err)
	return strings.Replace(endpoint, "127.0.0.1", "localhost", 1)
}

func TestRealPushGateway(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	prevPoll := service.PushPoll
	service.PushPoll = 100 * time.Millisecond
	t.Cleanup(func() { service.PushPoll = prevPoll })

	// The gateway needs the instance's port before the instance starts, to allow it.
	probe, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	gateway := startGateway(t, port)

	e := newEnv(t, func(o *envOpts) {
		o.publicHost = "host.docker.internal"
		o.listenPort = port
		o.mutate = func(c *config.Config) {
			c.Push = config.Push{Enabled: true, GatewayURL: gateway}
			c.MarkSet("push.enabled")
		}
	})
	owner := e.setup()
	origin := e.cfg.Server.PublicURL

	// The gateway verifies the instance through its well-known document.
	st := e.expect(200, e.do("POST", "/api/v1/instance/push/verify", owner, nil)).obj(t)
	require.Equal(t, "active", st["status"], st)
	require.Regexp(t, `^inst_`, st["instance_id"])

	// A phone registers with the gateway for this instance, then with the instance.
	bob, _ := e.register("bob")
	devBody, _ := json.Marshal(map[string]any{
		"app_id": "gotalk", "instance_origin": origin, "platform": "ios", "token": strings.Repeat("ab", 32), "apns_environment": "sandbox",
	})
	res, err := http.Post(gateway+"/v1/devices", "application/json", bytes.NewReader(devBody)) //nolint:noctx // test
	require.NoError(t, err)
	var dev map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&dev))
	_ = res.Body.Close()
	require.Equal(t, http.StatusCreated, res.StatusCode, dev)
	phone := newDevice(t)
	e.expect(201, e.do("POST", "/api/v1/users/@me/push-subscriptions", bob, map[string]any{
		"subscription_id": dev["subscription_id"], "app_id": "gotalk", "platform": "ios", "keys": phone.keys(),
		"gateway_url": gateway,
	}))

	// A mention on the instance becomes a push the gateway accepts.
	e.createPlace(owner, "news", "public")
	e.expect(200, e.do("POST", "/api/v1/places/news/join", bob, nil))
	general := e.board(owner, "news", map[string]any{"slug": "general", "name": "General"})
	e.topic(owner, general, "Launch", "Ready, @bob?")
	e.waitOutboxEmpty()

	var keyID string
	var seed []byte
	require.NoError(t, e.svc.Pool().QueryRow(context.Background(), "SELECT key_id, signing_key FROM push_gateway").Scan(&keyID, &seed))
	creds := pushgw.Credentials{KeyID: keyID, Key: ed25519Key(seed)}
	var inst *pushgw.Instance
	require.Eventually(t, func() bool {
		inst, err = pushgw.New(gateway, pushgw.Options{}).GetInstance(context.Background(), creds, st["instance_id"].(string))
		return err == nil && inst.Usage != nil && inst.Usage.PushesAccepted == 1
	}, 15*time.Second, 200*time.Millisecond, fmt.Sprint(inst, err))
	require.Equal(t, int64(1), inst.Usage.ActiveSubscriptions)
}

func ed25519Key(seed []byte) ed25519.PrivateKey { return ed25519.NewKeyFromSeed(seed) }
