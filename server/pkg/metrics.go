package pkg

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type Metrics struct {
	authSuccesses            *prometheus.CounterVec
	authFailures             *prometheus.CounterVec
	authErrors               *prometheus.CounterVec
	authInfrastructureErrors *prometheus.CounterVec
	authCacheEntries         prometheus.Gauge
	authCacheHits            prometheus.Counter
	authCacheMisses          prometheus.Counter
	authCacheInflightBackend prometheus.Gauge
	authCacheWaitingRequests prometheus.Gauge
	discoverySuccesses       *prometheus.CounterVec
	discoveryFailures        *prometheus.CounterVec
	discoveryErrors          *prometheus.CounterVec
	discoveryInfraErrors     *prometheus.CounterVec
	ytRequestError           *prometheus.CounterVec
	ytRequestDuration        *prometheus.HistogramVec
}

const (
	authReasonAuthorized        = "authorized"
	authReasonStaticBypass      = "static_bypass"
	authReasonTaskLookup        = "task_lookup"
	authReasonCredentials       = "credentials"
	authReasonUserNotIdentified = "user_not_identified"
	authReasonInvalidOperation  = "invalid_operation_id"
	authReasonPermissionDenied  = "permission_denied"
	authReasonInfrastructure    = "infra"
	discoveryReasonInfra        = "infra"

	grpcErrorContextDeadlineExceeded = "context_deadline_exceeded"
	grpcErrorContextCanceled         = "context_canceled"
	grpcErrorConnectionTimeout       = "connection_timeout"
	grpcErrorNetworkError            = "network_error"
	grpcErrorDeadlineExceeded        = "grpc_deadline_exceeded"
	grpcErrorCanceled                = "grpc_canceled"
	grpcErrorUnavailable             = "grpc_unavailable"
	grpcErrorGeneric                 = "grpc_error"
	grpcErrorOther                   = "other"
	grpcErrorCodeNone                = "none"
)

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	m := &Metrics{
		authSuccesses: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_success_total",
				Help: "Successful authorization outcomes grouped by reason.",
			},
			[]string{"reason"},
		),
		authFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_failed_total",
				Help: "Failed authorization outcomes grouped by reason.",
			},
			[]string{"reason"},
		),
		authErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_errors_total",
				Help: "Authorization-related errors grouped by stage.",
			},
			[]string{"stage"},
		),
		authInfrastructureErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_infra_errors_total",
				Help: "Infrastructure failures during authorization, grouped by stage and error class.",
			},
			[]string{"stage", "kind", "grpc_code"},
		),
		authCacheEntries: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "yt_task_proxy_auth_cache_entries",
				Help: "Current number of entries in auth cache.",
			},
		),
		authCacheHits: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_cache_hits_total",
				Help: "Total number of auth cache hits.",
			},
		),
		authCacheMisses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_auth_cache_misses_total",
				Help: "Total number of auth cache misses.",
			},
		),
		authCacheInflightBackend: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "yt_task_proxy_auth_cache_inflight_backend_requests",
				Help: "Current number of in-flight backend requests for auth cache.",
			},
		),
		authCacheWaitingRequests: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "yt_task_proxy_auth_cache_waiting_requests",
				Help: "Current number of requests waiting on in-flight auth cache backend requests.",
			},
		),
		discoverySuccesses: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_discovery_success_total",
				Help: "Successful discovery outcomes grouped by reason.",
			},
			[]string{"reason"},
		),
		discoveryFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_discovery_failed_total",
				Help: "Failed discovery outcomes grouped by reason.",
			},
			[]string{"reason"},
		),
		discoveryErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_discovery_errors_total",
				Help: "Discovery-related errors grouped by stage.",
			},
			[]string{"stage"},
		),
		discoveryInfraErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_discovery_infra_errors_total",
				Help: "Infrastructure failures during discovery, grouped by stage and error class.",
			},
			[]string{"stage", "kind", "grpc_code"},
		),
		ytRequestError: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "yt_task_proxy_ytsaurus_request_errors_total",
				Help: "YTsaurus request errors grouped by request kind and error class.",
			},
			[]string{"request", "kind", "grpc_code"},
		),
		ytRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "yt_task_proxy_ytsaurus_request_duration_seconds",
				Help:    "YTsaurus request duration grouped by request kind.",
				Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5},
			},
			[]string{"request"},
		),
	}

	registerer.MustRegister(
		m.authSuccesses,
		m.authFailures,
		m.authErrors,
		m.authInfrastructureErrors,
		m.authCacheEntries,
		m.authCacheHits,
		m.authCacheMisses,
		m.authCacheInflightBackend,
		m.authCacheWaitingRequests,
		m.discoverySuccesses,
		m.discoveryFailures,
		m.discoveryErrors,
		m.discoveryInfraErrors,
		m.ytRequestError,
		m.ytRequestDuration,
	)

	return m
}

func (m *Metrics) ObserveAuthSuccess(reason string) {
	m.authSuccesses.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveAuthFailure(reason string, err error) string {
	m.authErrors.WithLabelValues(reason).Inc()

	if err == nil {
		m.authFailures.WithLabelValues(reason).Inc()
		return reason
	}

	kind, grpcCode := classifyInfrastructureError(err)
	m.authInfrastructureErrors.WithLabelValues(reason, kind, grpcCode).Inc()

	failureReason := infrastructureAuthReason(kind)
	m.authFailures.WithLabelValues(failureReason).Inc()
	return failureReason
}

func (m *Metrics) ObserveYTError(request string, err error) string {
	kind, grpcCode := classifyInfrastructureError(err)
	m.ytRequestError.WithLabelValues(request, kind, grpcCode).Inc()
	return kind
}

func (m *Metrics) ObserveYTDuration(request string, duration time.Duration) {
	m.ytRequestDuration.WithLabelValues(request).Observe(duration.Seconds())
}

func (m *Metrics) ObserveAuthYTError(stage string, err error) string {
	m.ObserveYTError(stage, err)
	return m.ObserveAuthFailure(stage, err)
}

func (m *Metrics) ObserveAuthCacheHit() {
	m.authCacheHits.Inc()
}

func (m *Metrics) ObserveAuthCacheMiss() {
	m.authCacheMisses.Inc()
}

func (m *Metrics) IncAuthCacheEntries() {
	m.authCacheEntries.Inc()
}

func (m *Metrics) DecAuthCacheEntries() {
	m.authCacheEntries.Dec()
}

func (m *Metrics) IncAuthCacheInflightBackendRequests() {
	m.authCacheInflightBackend.Inc()
}

func (m *Metrics) DecAuthCacheInflightBackendRequests() {
	m.authCacheInflightBackend.Dec()
}

func (m *Metrics) IncAuthCacheWaitingRequests() {
	m.authCacheWaitingRequests.Inc()
}

func (m *Metrics) DecAuthCacheWaitingRequests() {
	m.authCacheWaitingRequests.Dec()
}

func (m *Metrics) ObserveDiscoverySuccess(reason string) {
	m.discoverySuccesses.WithLabelValues(reason).Inc()
}

func (m *Metrics) ObserveDiscoveryFailure(stage string, err error) string {
	m.discoveryErrors.WithLabelValues(stage).Inc()

	if err == nil {
		m.discoveryFailures.WithLabelValues(stage).Inc()
		return stage
	}

	kind, grpcCode := classifyInfrastructureError(err)
	m.discoveryInfraErrors.WithLabelValues(stage, kind, grpcCode).Inc()

	failureReason := infrastructureDiscoveryReason(kind)
	m.discoveryFailures.WithLabelValues(failureReason).Inc()
	return failureReason
}

func NewMetricsHandler(gatherer prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics/prometheus", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	return mux
}

func ServeMetrics(gatherer prometheus.Gatherer) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", metricsPort),
		Handler:           NewMetricsHandler(gatherer),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("metrics HTTP starts listening on :%d", metricsPort)

	return srv.ListenAndServe()
}

var defaultMetrics = NewMetrics(prometheus.DefaultRegisterer)

func DefaultMetrics() *Metrics {
	return defaultMetrics
}

func DefaultGatherer() prometheus.Gatherer {
	return prometheus.DefaultGatherer
}

func classifyInfrastructureError(err error) (string, string) {
	if err == nil {
		return grpcErrorOther, grpcErrorCodeNone
	}

	grpcCode := grpcErrorCodeNone
	if st, ok := grpcstatus.FromError(err); ok {
		grpcCode = grpcCodeLabel(st.Code())
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return grpcErrorContextDeadlineExceeded, grpcCode
	case errors.Is(err, context.Canceled):
		return grpcErrorContextCanceled, grpcCode
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return grpcErrorConnectionTimeout, grpcCode
		}
		return grpcErrorNetworkError, grpcCode
	}

	if st, ok := grpcstatus.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded:
			return grpcErrorDeadlineExceeded, grpcCode
		case codes.Canceled:
			return grpcErrorCanceled, grpcCode
		case codes.Unavailable:
			return grpcErrorUnavailable, grpcCode
		default:
			return grpcErrorGeneric, grpcCode
		}
	}

	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "connection timed out") {
		return grpcErrorConnectionTimeout, grpcCode
	}

	return grpcErrorOther, grpcCode
}

func infrastructureAuthReason(kind string) string {
	return authReasonInfrastructure + "_" + kind
}

func infrastructureDiscoveryReason(kind string) string {
	return discoveryReasonInfra + "_" + kind
}

func grpcCodeLabel(code codes.Code) string {
	name := code.String()
	var sb strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				sb.WriteByte('_')
			}
			sb.WriteRune(unicode.ToLower(r))
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
