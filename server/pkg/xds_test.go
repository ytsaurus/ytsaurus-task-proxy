package pkg

import (
	"sort"
	"strings"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

func TestMakeSnapshot(t *testing.T) {
	hashToTask := map[string]Task{
		"abc12345": {
			operationID:    "op123",
			operationAlias: "myalias",
			taskName:       "worker",
			service:        "api",
			protocol:       GRPC,
			jobs: []HostPort{
				{host: "10.0.0.1", port: 8080},
				{host: "10.0.0.2", port: 8080},
			},
		},
	}

	snapshot, err := makeSnapshot(hashToTask, "v1", "example.com", false, true, DefaultTaskProxyTimeoutConfig())
	require.NoError(t, err)

	// Convert snapshot to a structured map for YAML comparison
	snapshotMap := make(map[string]any)

	// Get clusters
	clusterResources := snapshot.GetResources(resourcev3.ClusterType)
	clusterNames := make([]string, 0, len(clusterResources))
	for name := range clusterResources {
		clusterNames = append(clusterNames, name)
	}
	sort.Strings(clusterNames) // Sort for deterministic output

	clusters := make([]map[string]any, 0, len(clusterResources))
	for _, name := range clusterNames {
		res := clusterResources[name]
		jsonBytes, err := protojson.Marshal(res)
		require.NoError(t, err)

		var clusterMap map[string]any
		err = yaml.Unmarshal(jsonBytes, &clusterMap)
		require.NoError(t, err)
		clusters = append(clusters, clusterMap)
	}
	snapshotMap["clusters"] = clusters

	// Get listener
	listenerResources := snapshot.GetResources(resourcev3.ListenerType)
	require.Len(t, listenerResources, 1)

	for _, res := range listenerResources {
		jsonBytes, err := protojson.Marshal(res)
		require.NoError(t, err)

		var listenerMap map[string]any
		err = yaml.Unmarshal(jsonBytes, &listenerMap)
		require.NoError(t, err)
		snapshotMap["listener"] = listenerMap
		break
	}

	resultYAML, err := yaml.Marshal(snapshotMap)
	require.NoError(t, err)

	expectedYAML := `clusters:
- connectTimeout: 2s
  loadAssignment:
    clusterName: extAuthz
    endpoints:
    - lbEndpoints:
      - endpoint:
          address:
            socketAddress:
              address: 127.0.0.1
              portValue: 9090
  name: extAuthz
  type: STATIC
  typedExtensionProtocolOptions:
    envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
      '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
      explicitHttpConfig:
        http2ProtocolOptions: {}
- connectTimeout: 2s
  loadAssignment:
    clusterName: op123-worker-api-0
    endpoints:
    - lbEndpoints:
      - endpoint:
          address:
            socketAddress:
              address: 10.0.0.1
              portValue: 8080
  name: op123-worker-api-0
  type: STRICT_DNS
  typedExtensionProtocolOptions:
    envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
      '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
      explicitHttpConfig:
        http2ProtocolOptions: {}
- connectTimeout: 2s
  loadAssignment:
    clusterName: op123-worker-api-1
    endpoints:
    - lbEndpoints:
      - endpoint:
          address:
            socketAddress:
              address: 10.0.0.2
              portValue: 8080
  name: op123-worker-api-1
  type: STRICT_DNS
  typedExtensionProtocolOptions:
    envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
      '@type': type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
      explicitHttpConfig:
        http2ProtocolOptions: {}
listener:
  accessLog:
  - name: envoy.access_loggers.stderr
    typedConfig:
      '@type': type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StderrAccessLog
  address:
    socketAddress:
      address: 0.0.0.0
      portValue: 8080
  filterChains:
  - filters:
    - name: envoy.filters.network.http_connection_manager
      typedConfig:
        '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
        http2ProtocolOptions: {}
        httpFilters:
        - name: envoy.filters.http.ext_authz
          typedConfig:
            '@type': type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz
            grpcService:
              envoyGrpc:
                clusterName: extAuthz
              timeout: 0.800s
        - name: envoy.filters.http.router
          typedConfig:
            '@type': type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
        routeConfig:
          name: local_routes
          virtualHosts:
          - domains:
            - abc12345.example.com
            - myalias-worker-api.example.com
            name: op123-worker-api
            routes:
            - match:
                prefix: /
              route:
                weightedClusters:
                  clusters:
                  - name: op123-worker-api-0
                    weight: 1
                  - name: op123-worker-api-1
                    weight: 1
          - domains:
            - '*'
            name: vhost_default
            routes:
            - match:
                headers:
                - name: x-yt-taskproxy-id
                  stringMatch:
                    exact: abc12345
                prefix: /
              route:
                weightedClusters:
                  clusters:
                  - name: op123-worker-api-0
                    weight: 1
                  - name: op123-worker-api-1
                    weight: 1
            - match:
                headers:
                - name: x-yt-taskproxy-operation-id
                  stringMatch:
                    exact: op123
                - name: x-yt-taskproxy-service
                  stringMatch:
                    exact: api
                - name: x-yt-taskproxy-task-name
                  stringMatch:
                    exact: worker
                prefix: /
              route:
                weightedClusters:
                  clusters:
                  - name: op123-worker-api-0
                    weight: 1
                  - name: op123-worker-api-1
                    weight: 1
            - match:
                headers:
                - name: x-yt-taskproxy-operation-alias
                  stringMatch:
                    exact: myalias
                - name: x-yt-taskproxy-service
                  stringMatch:
                    exact: api
                - name: x-yt-taskproxy-task-name
                  stringMatch:
                    exact: worker
                prefix: /
              route:
                weightedClusters:
                  clusters:
                  - name: op123-worker-api-0
                    weight: 1
                  - name: op123-worker-api-1
                    weight: 1
            - directResponse:
                body:
                  inlineString: no such task
                status: 404
              match:
                prefix: /
        statPrefix: ingress_http
        upgradeConfigs:
        - upgradeType: websocket
  name: listener_0
`

	expectedYAML = strings.ReplaceAll(
		expectedYAML,
		"route:\n                weightedClusters:",
		"route:\n                idleTimeout: 300s\n                timeout: 15s\n                weightedClusters:",
	)
	assert.YAMLEq(t, expectedYAML, string(resultYAML))
}

func TestMakeSnapshotTimeouts(t *testing.T) {
	zero := time.Duration(0)
	task := Task{
		operationID: "op123",
		taskName:    "worker",
		service:     "api",
		protocol:    HTTP,
		jobs:        []HostPort{{host: "10.0.0.1", port: 8080}},
		timeoutOverrides: TaskTimeoutOverrides{
			routeTimeout:      durationPtr(10 * time.Minute),
			streamIdleTimeout: &zero,
		},
	}
	config := TaskProxyTimeoutConfig{
		ConnectTimeout:    3 * time.Second,
		RouteTimeout:      15 * time.Second,
		StreamIdleTimeout: 5 * time.Minute,
	}

	snapshot, err := makeSnapshot(map[string]Task{"abc12345": task}, "v1", "example.com", false, false, config)
	require.NoError(t, err)

	cluster := snapshot.GetResources(resourcev3.ClusterType)["op123-worker-api-0"].(*clusterv3.Cluster)
	require.Equal(t, 3*time.Second, cluster.ConnectTimeout.AsDuration())

	listener := onlyListener(t, snapshot.GetResources(resourcev3.ListenerType))
	hcm := httpConnectionManager(t, listener)
	for _, vhost := range hcm.GetRouteConfig().GetVirtualHosts() {
		for _, route := range vhost.GetRoutes() {
			action := route.GetRoute()
			if action == nil || action.GetWeightedClusters() == nil {
				continue
			}
			require.Equal(t, 10*time.Minute, action.GetTimeout().AsDuration())
			require.Equal(t, time.Duration(0), action.GetIdleTimeout().AsDuration())
		}
	}
}

func onlyListener(t *testing.T, resources map[string]cachetypes.Resource) *listenerv3.Listener {
	t.Helper()
	require.Len(t, resources, 1)
	for _, resource := range resources {
		return resource.(*listenerv3.Listener)
	}
	return nil
}

func httpConnectionManager(t *testing.T, listener *listenerv3.Listener) *hcmv3.HttpConnectionManager {
	t.Helper()
	var hcm hcmv3.HttpConnectionManager
	err := listener.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&hcm)
	require.NoError(t, err)
	return &hcm
}
