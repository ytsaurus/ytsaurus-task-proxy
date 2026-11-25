package pkg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.ytsaurus.tech/yt/go/yt"
)

type userMetricsPermissionClient struct {
	yt.Client
}

func (userMetricsPermissionClient) CheckOperationPermission(_ context.Context, _ yt.OperationID, user string, _ yt.Permission, _ *yt.CheckOperationPermissionOptions) (*yt.CheckOperationPermissionResponse, error) {
	switch user {
	case "alice":
		resp := &yt.CheckOperationPermissionResponse{}
		resp.Action = "allow"
		return resp, nil
	case "bob":
		resp := &yt.CheckOperationPermissionResponse{}
		resp.Action = "deny"
		return resp, nil
	default:
		return nil, errors.New("permission backend unavailable")
	}
}

func TestAuthCheckCountsAuthenticatedUserRequests(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cache       bool
		whoamiCalls int32
	}{
		{name: "without auth cache", whoamiCalls: 6},
		{name: "with auth cache", cache: true, whoamiCalls: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var whoamiCalls atomic.Int32
			ytProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/auth/whoami" {
					t.Errorf("unexpected YT request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}

				whoamiCalls.Add(1)
				switch r.Header.Get("Authorization") {
				case "OAuth fixture-a":
					_, _ = io.WriteString(w, `{"login":"alice"}`)
				case "OAuth fixture-b":
					_, _ = io.WriteString(w, `{"login":"bob"}`)
				case "OAuth fixture-c":
					_, _ = io.WriteString(w, `{"login":"charlie"}`)
				default:
					_, _ = io.WriteString(w, `{"login":""}`)
				}
			}))
			defer ytProxy.Close()

			registry := prometheus.NewRegistry()
			auth := CreateAuthServer(userMetricsPermissionClient{}, strings.TrimPrefix(ytProxy.URL, "http://"), &SimpleLogger{}, "", AuthCacheConfig{Enabled: tc.cache})
			auth.metrics = NewMetrics(registry)
			auth.SetTasksData(map[string]Task{"task": {operationID: "1-2-3-4", service: "ui"}}, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, request := range []struct {
				host, path, credential string
				want                   *authv3.CheckResponse
			}{
				{"task", "/", "OAuth fixture-a", okResponse},
				{"task", "/", "OAuth fixture-a", okResponse},
				{"task", "/", "OAuth fixture-b", deniedResponse},
				{"task", "/", "OAuth fixture-b", deniedResponse},
				{"task", "/", "OAuth fixture-c", deniedResponse},
				{"task", "/", "OAuth unidentified", deniedResponse},
				{"task", "/", "", deniedResponse},
				{"task", "/static/image.png", "OAuth fixture-a", okResponse},
				{"unknown", "/", "OAuth fixture-a", deniedResponse},
			} {
				headers := map[string]string{"x-yt-user": "unverified"}
				if request.credential != "" {
					headers["authorization"] = request.credential
				}
				resp, err := auth.Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
					Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
						Host: request.host, Path: request.path, Headers: headers,
					}},
				}})
				require.NoError(t, err)
				require.Equal(t, request.want, resp)
			}

			require.Equal(t, tc.whoamiCalls, whoamiCalls.Load())
			require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP yt_task_proxy_user_requests_total Task Proxy authorization requests by authenticated YTsaurus user and rate limit decision.
# TYPE yt_task_proxy_user_requests_total counter
yt_task_proxy_user_requests_total{rate_limit="not_evaluated",user="alice"} 2
yt_task_proxy_user_requests_total{rate_limit="not_evaluated",user="bob"} 2
yt_task_proxy_user_requests_total{rate_limit="not_evaluated",user="charlie"} 1
`), "yt_task_proxy_user_requests_total"))
		})
	}
}
