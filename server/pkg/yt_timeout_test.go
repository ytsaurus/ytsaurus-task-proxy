package pkg

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

// A real HTTP endpoint exercises the SDK timeout and both authorization clients.
func delayedYTProxy(t *testing.T, delay func(string) time.Duration) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(delay(r.URL.Path))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		switch r.URL.Path {
		case "/auth/whoami":
			_, _ = io.WriteString(w, `{"login":"test-user"}`)
		case "/api/v4/check_operation_permission":
			_, _ = io.WriteString(w, `{action="allow";}`)
		default:
			t.Errorf("unexpected YT request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func TestYTClientLightRequestTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delay   time.Duration
		timeout time.Duration
		wantErr bool
	}{
		{"response within budget", 20 * time.Millisecond, time.Second, false},
		{"response exceeds budget", time.Second, 100 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := delayedYTProxy(t, func(string) time.Duration { return tc.delay })
			client, err := CreateYTClient(proxy, &ytsdk.TokenCredentials{Token: "test-token"}, tc.timeout, nil)
			require.NoError(t, err)
			t.Cleanup(client.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = client.WhoAmI(ctx, nil)
			if tc.wantErr {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.NoError(t, ctx.Err(), "SDK timeout must expire before the caller's timeout")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestYTClientRejectsInvalidTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		_, err := CreateYTClient("unused", nil, timeout, nil)
		require.ErrorContains(t, err, "yt-light-request-timeout must be positive")
	}
}

func TestAuthorizationYTClientTimeouts(t *testing.T) {
	for _, stalledPath := range []string{"", "/auth/whoami", "/api/v4/check_operation_permission"} {
		t.Run("stall="+stalledPath, func(t *testing.T) {
			proxy := delayedYTProxy(t, func(path string) time.Duration {
				if path == stalledPath {
					return time.Second
				}
				return 0
			})
			timeout := 100 * time.Millisecond
			client, err := CreateYTClient(proxy, &ytsdk.TokenCredentials{Token: "service-token"}, timeout, nil)
			require.NoError(t, err)
			t.Cleanup(client.Stop)
			auth := CreateAuthServer(client, proxy, timeout, &SimpleLogger{}, "", AuthCacheConfig{})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			allowed, err := auth.checkOperationPermission(ctx, "1-2-3-4", map[string]string{"authorization": "OAuth test-token"})
			if stalledPath == "" {
				require.NoError(t, err)
				require.True(t, allowed)
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.False(t, allowed)
				require.NoError(t, ctx.Err(), "configured SDK timeout must apply to both clients")
			}
		})
	}
}
