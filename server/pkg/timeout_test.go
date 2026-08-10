package pkg

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func durationPtr(value time.Duration) *time.Duration { return &value }

func TestValidateTaskProxyTimeoutConfig(t *testing.T) {
	valid := TaskProxyTimeoutConfig{
		ConnectTimeout:    2 * time.Second,
		RouteTimeout:      0,
		StreamIdleTimeout: 0,
	}
	require.NoError(t, valid.Validate())

	invalidConnect := valid
	invalidConnect.ConnectTimeout = 0
	require.ErrorContains(t, invalidConnect.Validate(), "connect timeout must be positive")

	invalidRoute := valid
	invalidRoute.RouteTimeout = -time.Second
	require.ErrorContains(t, invalidRoute.Validate(), "route timeout must be non-negative")

	invalidIdle := valid
	invalidIdle.StreamIdleTimeout = -time.Second
	require.ErrorContains(t, invalidIdle.Validate(), "stream idle timeout must be non-negative")
}

func TestDurationFromSeconds(t *testing.T) {
	duration, err := DurationFromSeconds(300)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, duration)

	_, err = DurationFromSeconds(-1)
	require.ErrorContains(t, err, "must be non-negative")

	_, err = DurationFromSeconds(int(math.MaxInt64/time.Second) + 1)
	require.ErrorContains(t, err, "is too large")
}
