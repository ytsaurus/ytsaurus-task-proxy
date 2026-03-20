package pkg

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindTaskByRequest(t *testing.T) {
	// Setup test data
	task1 := Task{
		operationID: "op-123",
		taskName:    "worker",
		service:     "api",
	}
	task1Hash := task1.Hash()

	task2 := Task{
		operationID:    "op-456",
		operationAlias: "myalias",
		taskName:       "master",
		service:        "ui",
	}
	task2Hash := task2.Hash()

	task3 := Task{
		operationID: "op-789",
		taskName:    "executor",
		service:     "grpc",
	}
	task3Hash := task3.Hash()

	hashToTasks := map[string]Task{
		task1Hash: task1,
		task2Hash: task2,
		task3Hash: task3,
	}

	operationAliasToID := map[string]string{
		"myalias":      "op-456",
		"anotheralias": "op-999",
	}

	server := CreateAuthServer(nil, "", &SimpleLogger{}, "")
	server.SetTasksData(hashToTasks, operationAliasToID)

	tests := []struct {
		name       string
		host       string
		headers    map[string]string
		expectedID string
		errorMsg   string
	}{
		// Source 1: Direct hash from x-yt-taskproxy-id header
		{
			name: "hash from header - valid task",
			host: "ignored.example.com",
			headers: map[string]string{
				"x-yt-taskproxy-id": task1Hash,
			},
			expectedID: task1.operationID,
		},
		{
			name: "hash from header - invalid hash",
			host: "ignored.example.com",
			headers: map[string]string{
				"x-yt-taskproxy-id": "nonexistent",
			},
			errorMsg: "no entry for hash \"nonexistent\" in tasks registry",
		},
		{
			name: "hash from header - empty hash",
			host: "ignored.example.com",
			headers: map[string]string{
				"x-yt-taskproxy-id": "",
			},
			errorMsg: "no entry for hash \"\" in tasks registry",
		},
		{
			name: "hash from header - header takes precedence over host",
			host: task3Hash + ".example.com",
			headers: map[string]string{
				"x-yt-taskproxy-id": task1Hash,
			},
			expectedID: task1.operationID,
		},

		// Source 2: Alias-based subdomain (format: alias-taskname-service)
		{
			name: "alias subdomain - valid alias",
			host: "myalias-master-ui.example.com",
			headers: map[string]string{
				"other-header": "value",
			},
			expectedID: task2.operationID,
		},
		{
			name:     "alias subdomain - unknown alias",
			host:     "unknownalias-master-ui.example.com",
			errorMsg: "operation by alias \"unknownalias\" from subdomain was not found",
		},
		{
			name:     "alias subdomain - valid alias but task not found",
			host:     "anotheralias-worker-api.example.com",
			errorMsg: "no entry for hash",
		},
		{
			name:       "alias subdomain - with port",
			host:       "myalias-master-ui.example.com:8080",
			expectedID: task2.operationID,
		},

		// Source 3: Direct hash from subdomain (fallback)
		{
			name:       "direct hash subdomain - valid hash",
			host:       task1Hash + ".example.com",
			expectedID: task1.operationID,
		},
		{
			name:     "direct hash subdomain - invalid hash",
			host:     "badhash.example.com",
			errorMsg: "no entry for hash \"badhash\" in tasks registry",
		},
		{
			name:       "direct hash subdomain - single part (no dots)",
			host:       task3Hash,
			expectedID: task3.operationID,
		},

		// Misc errors
		{
			name:     "invalid alias domain format",
			host:     "part1-part2.example.com",
			errorMsg: "no entry for hash \"part1-part2\" in tasks registry",
		},
		{
			name: "empty host with other headers",
			headers: map[string]string{
				"authorization": "Bearer token",
			},
			errorMsg: "authority (host) or x-yt-taskproxy-id headers are missing in request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := tt.headers
			if headers == nil {
				headers = map[string]string{}
			}
			task, err := server.findTaskByRequest(tt.host, headers)

			if tt.errorMsg != "" {
				require.ErrorContains(t, err, tt.errorMsg)
				assert.Nil(t, task)
			} else {
				require.NoError(t, err)
				require.NotNil(t, task)
				assert.Equal(t, tt.expectedID, task.operationID)
			}
		})
	}
}
