package pkg

import (
	"sort"
	"testing"

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

	snapshot, err := makeSnapshot(hashToTask, "v1", "example.com", false, true)
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

	assert.YAMLEq(t, expectedYAML, string(resultYAML))
}
