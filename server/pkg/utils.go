package pkg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	ytsdk "go.ytsaurus.tech/yt/go/yt"
	ythttpsdk "go.ytsaurus.tech/yt/go/yt/ythttp"
)

const (
	ytHTTPClientRetryCount    = 4
	ytHTTPClientReplayBodyMax = 1 << 20 // 1 MiB
)

type SimpleLogger struct{}

func (SimpleLogger) Debugf(format string, args ...any) {
	log.Printf("DEBUG: "+format, args...)
}
func (SimpleLogger) Infof(format string, args ...any) {
	log.Printf("INFO:  "+format, args...)
}
func (SimpleLogger) Warnf(format string, args ...any) {
	log.Printf("WARN:  "+format, args...)
}
func (SimpleLogger) Errorf(format string, args ...any) {
	log.Printf("ERROR: "+format, args...)
}

func CreateYTClient(proxy string, credentials ytsdk.Credentials, logger *SimpleLogger) (ytsdk.Client, error) {
	timeout := time.Second * 10
	cfg := &ytsdk.Config{
		Proxy:                 proxy,
		Credentials:           credentials,
		LightRequestTimeout:   &timeout,
		DisableProxyDiscovery: true,
	}

	httpClient, err := ythttpsdk.BuildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = &retryingRoundTripper{
		next:       httpClient.Transport,
		maxRetries: ytHTTPClientRetryCount,
		retryDelay: ytHTTPClientRetryDelay,
		logger:     logger,
	}
	cfg.HTTPClient = httpClient

	return ythttpsdk.NewClient(cfg)
}

type retryingRoundTripper struct {
	next       http.RoundTripper
	maxRetries int
	retryDelay func(retryAttempt int) time.Duration
	logger     *SimpleLogger
}

func (rt *retryingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	next := rt.next
	if next == nil {
		next = http.DefaultTransport
	}

	bodyFactory, contentLength, err := buildReplayableBodyFactory(req)
	if err != nil {
		return nil, err
	}
	canRetry := req.Body == nil || req.Body == http.NoBody || bodyFactory != nil
	if bodyFactory != nil {
		body, err := bodyFactory()
		if err != nil {
			return nil, err
		}
		req.Body = body
		req.GetBody = bodyFactory
		req.ContentLength = contentLength
	}

	for attempt := 0; ; attempt++ {
		resp, err := next.RoundTrip(req)
		if err == nil {
			return resp, nil
		}

		if attempt >= rt.maxRetries || !canRetry || !isRetriableHTTPClientError(err) {
			return nil, err
		}
		retryAttempt := attempt + 1
		delay := retryDelayForAttempt(rt.retryDelay, retryAttempt)
		if rt.logger != nil {
			rt.logger.Warnf(
				"retrying YTsaurus HTTP request %s %s due to retriable error (attempt %d/%d, backoff=%s): %v",
				req.Method,
				req.URL.String(),
				retryAttempt,
				rt.maxRetries,
				delay,
				err,
			)
		}

		retryReq := req.Clone(req.Context())
		if bodyFactory != nil {
			retryBody, bodyErr := bodyFactory()
			if bodyErr != nil {
				return nil, bodyErr
			}
			retryReq.Body = retryBody
			retryReq.GetBody = bodyFactory
			retryReq.ContentLength = contentLength
		}
		req = retryReq

		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		}
	}
}

func retryDelayForAttempt(retryDelay func(retryAttempt int) time.Duration, retryAttempt int) time.Duration {
	if retryDelay == nil {
		return 0
	}
	return retryDelay(retryAttempt)
}

func ytHTTPClientRetryDelay(retryAttempt int) time.Duration {
	switch retryAttempt {
	case 1:
		return 0
	case 2:
		return 10 * time.Millisecond
	case 3:
		return 20 * time.Millisecond
	default:
		return 30 * time.Millisecond
	}
}

func isRetriableHTTPClientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "server closed idle connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		(strings.Contains(msg, "contentlength=") && strings.Contains(msg, "body length 0"))
}

func buildReplayableBodyFactory(req *http.Request) (func() (io.ReadCloser, error), int64, error) {
	if req.Body == nil || req.Body == http.NoBody || req.GetBody == nil {
		return nil, 0, nil
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(body, ytHTTPClientReplayBodyMax+1))
	if err != nil {
		return nil, 0, err
	}
	if len(payload) > ytHTTPClientReplayBodyMax {
		return nil, 0, nil
	}

	contentLength := int64(len(payload))
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}, contentLength, nil
}
