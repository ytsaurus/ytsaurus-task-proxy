package pkg

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTaskProxyAnnotation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		annotation any
		expected   []taskServiceInfo
	}{
		{
			name: "full annotation",
			annotation: map[string]any{
				taskProxyEnabledKey: true,
				taskProxyTasksInfoKey: map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							taskProxyProtocolKey:  "grpc",
							taskProxyPortIndexKey: 0,
						},
					},
				},
			},
			expected: []taskServiceInfo{
				{
					task:      "example_grpc_server",
					service:   "server",
					protocol:  GRPC,
					portIndex: 0,
				},
			},
		},
		{
			name: "minimal annotation",
			annotation: map[string]any{
				taskProxyEnabledKey: true,
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "disabled annotation (false)",
			annotation: map[string]any{
				taskProxyEnabledKey: false,
			},
			expected: nil,
		},
		{
			name:       "disabled annotation (no attribute)",
			annotation: map[string]any{},
			expected:   nil,
		},
		{
			name:       "disabled annotation (nil)",
			annotation: nil,
			expected:   nil,
		},
		{
			name: "unknown protocol",
			annotation: map[string]any{
				taskProxyEnabledKey: true,
				taskProxyTasksInfoKey: map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							taskProxyProtocolKey:  "dns",
							taskProxyPortIndexKey: 0,
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "invalid port type",
			annotation: map[string]any{
				taskProxyEnabledKey: true,
				taskProxyTasksInfoKey: map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							taskProxyProtocolKey:  "http",
							taskProxyPortIndexKey: "0",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "missing service attributes",
			annotation: map[string]any{
				taskProxyEnabledKey: true,
				taskProxyTasksInfoKey: map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							taskProxyProtocolKey: "http",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			taskServiceInfos, _ := parseTaskProxyAnnotation(tt.annotation)
			assert.Equal(t, tt.expected, taskServiceInfos)
		})
	}
}

func TestParseInteger(t *testing.T) {
	for _, value := range []any{int(1), int64(1), int32(1), int16(1), int8(1), uint64(1), uint32(1), uint16(1), uint8(1)} {
		parsed, ok := parseInteger(value)
		require.True(t, ok)
		require.EqualValues(t, 1, parsed)
	}

	_, ok := parseInteger(uint64(math.MaxInt64) + 1)
	require.False(t, ok)
	_, ok = parseInteger("1")
	require.False(t, ok)
}

func TestParseTaskProxyAnnotationTimeoutOverrides(t *testing.T) {
	annotation := map[string]any{
		taskProxyEnabledKey:             true,
		taskProxyRouteTimeoutSecondsKey: 600,
		taskProxyStreamIdleTimeoutKey:   120,
	}

	_, overrides := parseTaskProxyAnnotation(annotation)

	require.Equal(t, durationPtr(10*time.Minute), overrides.routeTimeout)
	require.Equal(t, durationPtr(2*time.Minute), overrides.streamIdleTimeout)
}

func TestParseTaskProxyAnnotationRejectsInvalidTimeoutOverrides(t *testing.T) {
	annotation := map[string]any{
		taskProxyEnabledKey:             true,
		taskProxyRouteTimeoutSecondsKey: -1,
	}

	services, _ := parseTaskProxyAnnotation(annotation)

	assert.Nil(t, services)
}
