package pkg

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestMetricsHandler(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.ObserveAuthSuccess(authReasonAuthorized)
	metrics.ObserveAuthFailure(authReasonPermissionDenied, nil)
	metrics.ObserveAuthYTError("permission_check", context.DeadlineExceeded)
	metrics.ObserveAuthFailure(authReasonTaskLookup, nil)
	metrics.ObserveYTError("list_operations", grpcstatus.Error(codes.Unavailable, "backend unavailable"))
	metrics.ObserveYTDuration("whoami", 75*time.Millisecond)
	metrics.ObserveYTDuration("list_operations", 250*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	rec := httptest.NewRecorder()

	NewMetricsHandler(registry).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	require.True(t, strings.Contains(body, `auth_success_total{reason="authorized"} 1`))
	require.True(t, strings.Contains(body, `auth_failed_total{reason="permission_denied"} 1`))
	require.True(t, strings.Contains(body, `auth_failed_total{reason="infra_context_deadline_exceeded"} 1`))
	require.True(t, strings.Contains(body, `auth_errors_total{stage="task_lookup"} 1`))
	require.True(t, strings.Contains(body, `auth_errors_total{stage="permission_check"} 1`))
	require.True(t, strings.Contains(body, `auth_infra_errors_total{grpc_code="none",kind="context_deadline_exceeded",stage="permission_check"} 1`))
	require.True(t, strings.Contains(body, `ytsaurus_request_errors_total{grpc_code="unavailable",kind="grpc_unavailable",request="list_operations"} 1`))
	require.True(t, strings.Contains(body, `ytsaurus_request_duration_seconds_bucket{request="whoami",le="0.1"} 1`))
	require.True(t, strings.Contains(body, `ytsaurus_request_duration_seconds_count{request="whoami"} 1`))
	require.True(t, strings.Contains(body, `ytsaurus_request_duration_seconds_bucket{request="list_operations",le="0.3"} 1`))
	require.True(t, strings.Contains(body, `ytsaurus_request_duration_seconds_count{request="list_operations"} 1`))
}

func TestObserveAuthFailureInfrastructureClassification(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	require.Equal(t, "infra_context_canceled", metrics.ObserveAuthYTError("whoami", errors.Join(context.Canceled, errors.New("request canceled"))))
	require.Equal(t, "infra_connection_timeout", metrics.ObserveAuthYTError("permission_check", &net.DNSError{IsTimeout: true, Err: "i/o timeout"}))
	require.Equal(t, "infra_grpc_deadline_exceeded", metrics.ObserveAuthYTError("permission_check", grpcstatus.Error(codes.DeadlineExceeded, "deadline exceeded")))
	require.Equal(t, "infra_grpc_unavailable", metrics.ObserveAuthYTError("permission_check", grpcstatus.Error(codes.Unavailable, "connection refused")))

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	rec := httptest.NewRecorder()
	NewMetricsHandler(registry).ServeHTTP(rec, req)

	body := rec.Body.String()
	require.True(t, strings.Contains(body, `auth_failed_total{reason="infra_context_canceled"} 1`))
	require.True(t, strings.Contains(body, `auth_failed_total{reason="infra_connection_timeout"} 1`))
	require.True(t, strings.Contains(body, `auth_failed_total{reason="infra_grpc_deadline_exceeded"} 1`))
	require.True(t, strings.Contains(body, `auth_failed_total{reason="infra_grpc_unavailable"} 1`))
	require.True(t, strings.Contains(body, `auth_infra_errors_total{grpc_code="none",kind="context_canceled",stage="whoami"} 1`))
	require.True(t, strings.Contains(body, `auth_infra_errors_total{grpc_code="none",kind="connection_timeout",stage="permission_check"} 1`))
	require.True(t, strings.Contains(body, `auth_infra_errors_total{grpc_code="deadline_exceeded",kind="grpc_deadline_exceeded",stage="permission_check"} 1`))
	require.True(t, strings.Contains(body, `auth_infra_errors_total{grpc_code="unavailable",kind="grpc_unavailable",stage="permission_check"} 1`))
}
