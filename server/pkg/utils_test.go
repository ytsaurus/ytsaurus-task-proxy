package pkg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sequenceRoundTripper struct {
	errs       []error
	calls      int
	seenBodies []string
}

func (s *sequenceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		s.seenBodies = append(s.seenBodies, string(body))
	} else {
		s.seenBodies = append(s.seenBodies, "")
	}

	if s.calls <= len(s.errs) && s.errs[s.calls-1] != nil {
		return nil, s.errs[s.calls-1]
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestRetryingRoundTripperRetriesEOFAndReplaysBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "eof",
			err: &url.Error{
				Op:  "post",
				URL: "http://http-proxies-lb.yt.svc.cluster.local/api/v4/check_operation_permission",
				Err: io.EOF,
			},
		},
		{
			name: "server closed idle connection",
			err: &url.Error{
				Op:  "post",
				URL: "http://http-proxies-lb.yt.svc.cluster.local/api/v4/check_operation_permission",
				Err: errors.New("http: Server closed idle connection"),
			},
		},
		{
			name: "connection reset by peer",
			err:  errors.New(`post "http://http-proxies-lb.yt.svc.cluster.local/api/v4/check_operation_permission": read tcp 10.4.243.9:58314->10.247.73.39:80: read: connection reset by peer`),
		},
		{
			name: "content length mismatch",
			err:  errors.New(`post "http://http-proxies-lb.yt.svc.cluster.local/api/v4/check_operation_permission": : http: ContentLength=85 with Body length 0`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"op":"check_permission"}`
			params := bytes.NewBufferString(payload)

			req, err := http.NewRequest(http.MethodPost, "http://example.local/api/v4/check_operation_permission", http.NoBody)
			require.NoError(t, err)
			req.Body = io.NopCloser(params)
			req.ContentLength = int64(params.Len())
			req.GetBody = func() (io.ReadCloser, error) {
				// Emulate YTsaurus SDK request body factory: it returns the same buffer each call.
				return io.NopCloser(params), nil
			}

			next := &sequenceRoundTripper{errs: []error{tc.err}}
			rt := &retryingRoundTripper{
				next:       next,
				maxRetries: 1,
				retryDelay: func(int) time.Duration {
					return 0
				},
			}
			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, 2, next.calls)
			require.Equal(t, []string{payload, payload}, next.seenBodies)
		})
	}
}

func TestRetryingRoundTripperDoesNotRetryNonRetriableError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "generic non-retriable",
			err:  errors.New("bad request format"),
		},
		{
			name: "context canceled from log",
			err:  context.Canceled,
		},
		{
			name: "context deadline exceeded from log",
			err:  context.DeadlineExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://example.local/api/v4/list_operations", nil)
			require.NoError(t, err)

			next := &sequenceRoundTripper{errs: []error{tc.err}}
			rt := &retryingRoundTripper{
				next:       next,
				maxRetries: 3,
				retryDelay: func(int) time.Duration {
					return 0
				},
			}
			resp, err := rt.RoundTrip(req)
			require.Error(t, err)
			require.Nil(t, resp)
			require.Equal(t, 1, next.calls)
		})
	}
}

func TestYTHTTPClientRetryDelay(t *testing.T) {
	require.Equal(t, 0*time.Millisecond, ytHTTPClientRetryDelay(1))
	require.Equal(t, 10*time.Millisecond, ytHTTPClientRetryDelay(2))
	require.Equal(t, 20*time.Millisecond, ytHTTPClientRetryDelay(3))
	require.Equal(t, 30*time.Millisecond, ytHTTPClientRetryDelay(4))
}
