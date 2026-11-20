package pkg

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/require"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
	"google.golang.org/grpc"
)

// Opt in with TASK_PROXY_ENVOY_IMAGE=envoyproxy/envoy:v1.36-latest.
// Docker must provide host.docker.internal (Docker Desktop or Colima on macOS).
func TestEnvoyAuthorizationTimeout(t *testing.T) {
	image := os.Getenv("TASK_PROXY_ENVOY_IMAGE")
	if image == "" {
		t.Skip("set TASK_PROXY_ENVOY_IMAGE to run the Docker/Envoy integration test")
	}
	var delayNanos atomic.Int64
	var whoAmICalls atomic.Int64
	proxy := delayedYTProxy(t, func(path string) time.Duration {
		if path == "/auth/whoami" {
			whoAmICalls.Add(1)
			return time.Duration(delayNanos.Load())
		}
		return 0
	})
	config := DefaultTaskProxyTimeoutConfig()
	ytLightRequestTimeout := 5 * time.Second
	client, err := CreateYTClient(proxy, &ytsdk.TokenCredentials{Token: "service-token"}, ytLightRequestTimeout, nil)
	require.NoError(t, err)
	t.Cleanup(client.Stop)
	auth := CreateAuthServer(client, proxy, ytLightRequestTimeout, &SimpleLogger{}, "", AuthCacheConfig{Enabled: true, TTLSeconds: 30})
	task := Task{operationID: "1-2-3-4", taskName: "worker", service: "api", protocol: HTTP}
	tasks := map[string]Task{task.Hash(): task}
	auth.SetTasksData(tasks, nil)

	lis, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	gs := grpc.NewServer()
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(gs, serverv3.NewServer(ctx, cache, nil))
	authv3.RegisterAuthorizationServer(gs, auth)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	grpcPort := lis.Addr().(*net.TCPAddr).Port

	bootstrap := fmt.Sprintf(`{
  "node":{"cluster":"test","id":"id"},
  "dynamic_resources":{
    "ads_config":{"api_type":"GRPC","transport_api_version":"V3","grpc_services":[{"envoy_grpc":{"cluster_name":"xds"}}]},
    "lds_config":{"ads":{},"resource_api_version":"V3"},
    "cds_config":{"ads":{},"resource_api_version":"V3"}
  },
  "static_resources":{"clusters":[{
    "name":"xds","type":"STRICT_DNS","connect_timeout":"2s","http2_protocol_options":{},
    "load_assignment":{"cluster_name":"xds","endpoints":[{"lb_endpoints":[{"endpoint":{"address":{"socket_address":{"address":"host.docker.internal","port_value":%d}}}}]}]}
  }]},
  "admin":{"address":{"socket_address":{"address":"0.0.0.0","port_value":9901}}}
}`, grpcPort)
	docker := func(args ...string) string {
		t.Helper()
		cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cmdCancel()
		out, err := exec.CommandContext(cmdCtx, "docker", args...).CombinedOutput()
		require.NoError(t, err, "docker %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	id := docker("run", "-d", "--rm", "-p", "127.0.0.1::8080", "-p", "127.0.0.1::9901", image, "--config-yaml", bootstrap, "--concurrency", "1", "--log-level", "warn")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(docker("logs", id))
		}
		docker("rm", "-f", id)
	})
	url := "http://" + docker("port", id, "8080/tcp")
	adminURL := "http://" + docker("port", id, "9901/tcp")
	httpClient := &http.Client{Timeout: 6 * time.Second}
	t.Cleanup(httpClient.CloseIdleConnections)

	for i, tc := range []struct {
		name    string
		timeout time.Duration
		delay   time.Duration
		status  int
	}{
		{"old budget rejects late response", 800 * time.Millisecond, time.Second, http.StatusForbidden},
		{"increased budget accepts late response", 2 * time.Second, time.Second, http.StatusNotFound},
		{"new budget exceeded", 2 * time.Second, 3 * time.Second, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reuse the version when only the stub's delay changes, not the listener config.
			version := tc.timeout.String()
			config.AuthCheckTimeout = tc.timeout
			snapshot, err := makeSnapshot(nil, version, "example.com", false, true, config)
			require.NoError(t, err)
			// Only the test's network address differs from the production snapshot.
			cluster := snapshot.GetResources(resourcev3.ClusterType)[extAuthClusterName].(*clusterv3.Cluster)
			cluster.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS}
			socket := cluster.LoadAssignment.Endpoints[0].LbEndpoints[0].GetEndpoint().Address.GetSocketAddress()
			socket.Address = "host.docker.internal"
			socket.PortSpecifier = &corev3.SocketAddress_PortValue{PortValue: uint32(grpcPort)}
			require.NoError(t, cache.SetSnapshot(ctx, NodeID, snapshot))
			// Independently read the timeout actually accepted by Envoy via config_dump.
			require.Eventually(t, func() bool {
				resp, err := httpClient.Get(adminURL + "/config_dump")
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				timeout, err := authCheckTimeoutInConfigDump(body, "listener_0", version)
				return err == nil && timeout == tc.timeout
			}, 15*time.Second, 100*time.Millisecond)
			// An existing downstream connection can retain the previous listener's filter config.
			// Each case must use the listener whose active config was just read back.
			httpClient.CloseIdleConnections()
			delayNanos.Store(int64(tc.delay))
			doRequest := func() int {
				req, err := http.NewRequest(http.MethodGet, url+"/", nil)
				require.NoError(t, err)
				req.Header.Set(idRouterHeaderName, task.Hash())
				req.Header.Set("Authorization", fmt.Sprintf("OAuth test-token-%d", i))
				resp, err := httpClient.Do(req)
				require.NoError(t, err)
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			started := time.Now()
			require.Equal(t, tc.status, doRequest())
			if tc.status == http.StatusForbidden {
				require.InDelta(t, tc.timeout.Seconds(), time.Since(started).Seconds(), 0.5, "Envoy must stop at its configured authorization deadline")
			}
			t.Logf("auth timeout=%s, YT delay=%s, HTTP status=%d, elapsed=%s", tc.timeout, tc.delay, tc.status, time.Since(started))
			if tc.status == http.StatusNotFound {
				// 404 comes from the generated fallback route, after successful auth.
				calls := whoAmICalls.Load()
				require.Equal(t, tc.status, doRequest())
				require.Equal(t, calls, whoAmICalls.Load(), "successful delayed response must fill the cache")
			}
		})
	}
}
