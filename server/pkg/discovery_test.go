package pkg

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "grpc",
							"port_index": 0,
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
				"enabled": true,
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "disabled annotation (false)",
			annotation: map[string]any{
				"enabled": false,
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
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "dns",
							"port_index": 0,
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "invalid port type",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "http",
							"port_index": "0",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "missing service attributes",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol": "http",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			taskServiceInfos := parseTaskProxyAnnotation(tt.annotation)
			assert.Equal(t, tt.expected, taskServiceInfos)
		})
	}
}
