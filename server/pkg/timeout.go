package pkg

import (
	"fmt"
	"math"
	"time"
)

const (
	defaultConnectTimeout    = 2 * time.Second
	defaultRouteTimeout      = 15 * time.Second
	defaultStreamIdleTimeout = 5 * time.Minute
)

// TaskProxyTimeoutConfig controls timeout behavior for every task-proxy route.
type TaskProxyTimeoutConfig struct {
	// ConnectTimeout limits how long Envoy waits to establish a TCP connection to a job.
	// It must be positive because Envoy requires a connect timeout for every cluster.
	ConnectTimeout time.Duration
	// RouteTimeout limits the total time Envoy waits for an upstream response after it receives the full request.
	// Zero disables this timeout, which is useful for long-lived streaming responses.
	RouteTimeout time.Duration
	// StreamIdleTimeout limits a request or response stream with no upstream or downstream traffic.
	// Zero disables this timeout; active traffic keeps the stream alive regardless of this value.
	StreamIdleTimeout time.Duration
}

func DefaultTaskProxyTimeoutConfig() TaskProxyTimeoutConfig {
	return TaskProxyTimeoutConfig{
		ConnectTimeout:    defaultConnectTimeout,
		RouteTimeout:      defaultRouteTimeout,
		StreamIdleTimeout: defaultStreamIdleTimeout,
	}
}

func (c TaskProxyTimeoutConfig) Validate() error {
	if c.ConnectTimeout <= 0 {
		return fmt.Errorf("connect timeout must be positive")
	}
	if c.RouteTimeout < 0 {
		return fmt.Errorf("route timeout must be non-negative")
	}
	if c.StreamIdleTimeout < 0 {
		return fmt.Errorf("stream idle timeout must be non-negative")
	}
	return nil
}

// DurationFromSeconds converts a non-negative whole-second setting without allowing time.Duration overflow.
func DurationFromSeconds(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("timeout seconds must be non-negative")
	}
	if uint64(seconds) > uint64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("timeout seconds value %d is too large", seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

type TaskTimeoutOverrides struct {
	routeTimeout      *time.Duration
	streamIdleTimeout *time.Duration
}

func (o TaskTimeoutOverrides) routeTimeoutOr(defaultValue time.Duration) time.Duration {
	if o.routeTimeout != nil {
		return *o.routeTimeout
	}
	return defaultValue
}

func (o TaskTimeoutOverrides) streamIdleTimeoutOr(defaultValue time.Duration) time.Duration {
	if o.streamIdleTimeout != nil {
		return *o.streamIdleTimeout
	}
	return defaultValue
}
