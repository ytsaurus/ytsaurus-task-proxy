package pkg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseTaskProxyAnnotation(t *testing.T) {
	annotation := map[string]any{
		"enabled": true,
		"tasks_info": map[string]any{
			"example_grpc_server": map[string]any{
				"server": map[string]any{
					"protocol":   "grpc",
					"port_index": 0,
				},
			},
		},
	}
	taskServiceInfos := parseTaskProxyAnnotation(annotation)

	assert.Equal(t, []taskServiceInfo{
		{
			task:      "example_grpc_server",
			service:   "server",
			protocol:  GRPC,
			portIndex: 0,
		},
	}, taskServiceInfos)
}
