package pkg

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func authCheckTimeoutInConfigDump(body []byte, listenerName, version string) (time.Duration, error) {
	// Other config dump sections may contain extension types not linked into the test binary.
	var dump struct {
		Configs []struct {
			Type             string `json:"@type"`
			VersionInfo      string `json:"version_info"`
			DynamicListeners []struct {
				Name        string `json:"name"`
				ActiveState *struct {
					Listener json.RawMessage `json:"listener"`
				} `json:"active_state"`
			} `json:"dynamic_listeners"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(body, &dump); err != nil {
		return 0, err
	}
	for _, config := range dump.Configs {
		if config.Type != "type.googleapis.com/envoy.admin.v3.ListenersConfigDump" || config.VersionInfo != version {
			continue
		}
		for _, dynamic := range config.DynamicListeners {
			active := dynamic.ActiveState
			// In-place listener updates can retain active_state.version_info while
			// updating the listener itself. Check the LDS response version above.
			if dynamic.Name != listenerName || active == nil {
				continue
			}
			var listener listenerv3.Listener
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(active.Listener, &listener); err != nil {
				return 0, err
			}
			for _, chain := range listener.FilterChains {
				for _, filter := range chain.Filters {
					if filter.Name != "envoy.filters.network.http_connection_manager" {
						continue
					}
					var hcm hcmv3.HttpConnectionManager
					if err := filter.GetTypedConfig().UnmarshalTo(&hcm); err != nil {
						return 0, err
					}
					for _, httpFilter := range hcm.HttpFilters {
						if httpFilter.Name != "envoy.filters.http.ext_authz" {
							continue
						}
						var authz extauthzv3.ExtAuthz
						if err := httpFilter.GetTypedConfig().UnmarshalTo(&authz); err != nil {
							return 0, err
						}
						return authz.GetGrpcService().GetTimeout().AsDuration(), nil
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("active listener %q with ext_authz at LDS version %q not found", listenerName, version)
}

func TestAuthCheckTimeoutInConfigDump(t *testing.T) {
	state := func(version string, timeout time.Duration) map[string]any {
		config := DefaultTaskProxyTimeoutConfig()
		config.AuthCheckTimeout = timeout
		snapshot, err := makeSnapshot(nil, version, "example.com", false, true, config)
		require.NoError(t, err)
		listener, err := protojson.Marshal(snapshot.GetResources(resourcev3.ListenerType)["listener_0"])
		require.NoError(t, err)
		return map[string]any{"version_info": version, "listener": json.RawMessage(listener)}
	}
	active := state("old", 800*time.Millisecond)
	newState := state("new", 2*time.Second)
	for _, tc := range []struct {
		name          string
		states        map[string]any
		configVersion string
		version       string
		want          time.Duration
		wantErr       bool
	}{
		{"warming only", map[string]any{"warming_state": newState}, "new", "new", 0, true},
		{"draining only", map[string]any{"draining_state": []any{newState}}, "new", "new", 0, true},
		{"active wins over warming and draining", map[string]any{"active_state": active, "warming_state": newState, "draining_state": []any{newState}}, "new", "new", 800 * time.Millisecond, false},
		{"old LDS response", map[string]any{"active_state": active, "warming_state": newState}, "old", "new", 0, true},
		{"new active version", map[string]any{"active_state": newState, "draining_state": []any{active}}, "new", "new", 2 * time.Second, false},
		{"in-place update retains listener version", map[string]any{"active_state": state("old", 2*time.Second)}, "new", "new", 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := map[string]any{"name": "listener_0"}
			for key, value := range tc.states {
				listener[key] = value
			}
			dump, err := json.Marshal(map[string]any{"configs": []any{map[string]any{
				"@type":        "type.googleapis.com/envoy.admin.v3.ListenersConfigDump",
				"version_info": tc.configVersion,
				"dynamic_listeners": []any{
					map[string]any{"name": "other_listener", "active_state": newState},
					listener,
				},
			}}})
			require.NoError(t, err)
			timeout, err := authCheckTimeoutInConfigDump(dump, "listener_0", tc.version)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, timeout)
			}
		})
	}
}
