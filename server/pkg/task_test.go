package pkg

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateTask(t *testing.T) {
	for _, tt := range []struct {
		name string
		task Task
		err  error
	}{
		{
			name: "valid",
			task: Task{
				operationID:    "123",
				operationAlias: "alias",
				taskName:       "task",
				service:        "service",
			},
		},
		{
			name: "invalid alias",
			task: Task{
				operationID:    "123",
				operationAlias: "ali-as",
				taskName:       "task",
				service:        "service",
			},
			err: errors.New("field \"operationAlias\" value \"ali-as\" does not match regexp \"^[a-z0-9]+$\""),
		},
		{
			name: "invalid task name",
			task: Task{
				operationID:    "123",
				operationAlias: "alias",
				taskName:       "Task",
				service:        "service",
			},
			err: errors.New("field \"taskName\" value \"Task\" does not match regexp \"^[a-z0-9]+$\""),
		},
		{
			name: "invalid service",
			task: Task{
				operationID:    "123",
				operationAlias: "alias",
				taskName:       "task",
				service:        "$ervice",
			},
			err: errors.New("field \"service\" value \"$ervice\" does not match regexp \"^[a-z0-9]+$\""),
		},
		{
			name: "do not check if no alias",
			task: Task{
				operationID: "123-456",
				taskName:    "Task",
				service:     "$ervice",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.err, tt.task.Validate())
		})
	}
}
