package pkg

import (
	"context"
	"net/http"
	"strings"
	"sync"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.ytsaurus.tech/yt/go/guid"
	"go.ytsaurus.tech/yt/go/yt"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

type authServer struct {
	authv3.UnimplementedAuthorizationServer

	mx             sync.RWMutex
	hashToTasks    map[string]Task
	yt             ytsdk.Client
	ytProxy        string
	logger         *SimpleLogger
	authCookieName string
}

func CreateAuthServer(yt ytsdk.Client, ytProxy string, logger *SimpleLogger, authCookieName string) *authServer {
	return &authServer{
		hashToTasks:    make(map[string]Task),
		mx:             sync.RWMutex{},
		yt:             yt,
		ytProxy:        ytProxy,
		logger:         logger,
		authCookieName: authCookieName,
	}
}

func (s *authServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpAttrs := req.GetAttributes().GetRequest().GetHttp()
	path := httpAttrs.GetPath()
	headers := httpAttrs.GetHeaders()

	host := httpAttrs.Host
	if host == "" {
		s.logger.Warnf("authority (host) header is missing in request")
		return deniedResponse, nil
	}

	s.logger.Debugf("checking auth for host %q, path %q", host, path)

	hash := strings.Split(host, ".")[0]
	task, ok := s.getHashToTasks()[hash]
	if !ok {
		s.logger.Warnf("no entry for host %q in tasks registry", host)
		return deniedResponse, nil
	}

	// skip auth for UI services for statics; currently it is the case for SPYT UI
	if task.service == "ui" && strings.HasPrefix(path, "/static") {
		s.logger.Debugf("skip auth for 'ui' service for statics on path %s", path)
		return okResponse, nil
	}

	s.logger.Debugf("auth for host %q, path %q, task %v", host, path, task)

	allowed, err := s.checkOperationPermission(ctx, task.operationID, headers)
	if err != nil {
		s.logger.Errorf("error while checking operation permission: %v", err)
		return deniedResponse, nil
	}

	if !allowed {
		return deniedResponse, nil
	}
	return okResponse, nil
}

func (s *authServer) SetHashToTasks(hashToTasks map[string]Task) {
	s.mx.Lock()
	defer s.mx.Unlock()

	s.hashToTasks = hashToTasks
}

func (s *authServer) getHashToTasks() map[string]Task {
	s.mx.RLock()
	defer s.mx.RUnlock()

	return s.hashToTasks
}

// TODO: temporary implementation, use YT Go SDK instead
func (s *authServer) checkOperationPermission(ctx context.Context, operationID string, headers map[string]string) (bool, error) {
	userCredentials := s.getYTCredentialsFromHeaders(headers)
	if userCredentials == nil {
		return false, nil
	}

	userYT, err := CreateYTClient(s.ytProxy, userCredentials)
	if err != nil {
		return false, err
	}

	userResp, err := userYT.WhoAmI(ctx, nil)
	if err != nil {
		return false, err
	}

	user := userResp.Login
	if user == "" {
		s.logger.Warnf("user not identified by provided credentials")
		return false, nil
	}
	s.logger.Debugf("auth user is %q", user)

	operationIDg, err := guid.ParseString(operationID)
	if err != nil {
		s.logger.Warnf("invalid operation ID %s", operationID)
		return false, nil
	}

	resp, err := s.yt.CheckOperationPermission(
		ctx,
		yt.OperationID(operationIDg),
		user,
		yt.PermissionRead,
		nil,
	)
	if err != nil {
		return false, err
	}

	s.logger.Debugf("check operation permission result is %q for user %q and operation %q", resp.Action, user, operationID)
	return resp.Action == "allow", nil
}

func (s *authServer) getYTCredentialsFromHeaders(headers map[string]string) ytsdk.Credentials {
	if auth, ok := headers["authorization"]; ok {
		parts := strings.Split(auth, " ")
		if len(parts) != 2 {
			s.logger.Warnf("invalid authorization header value")
			return nil
		}
		name := strings.ToLower(parts[0])
		value := parts[1]

		switch name {
		case "oauth":
			s.logger.Debugf("user authorization is OAuth token")
			return &ytsdk.TokenCredentials{Token: value}
		case "bearer":
			s.logger.Debugf("user authorization is Bearer token")
			return &ytsdk.BearerCredentials{Token: value}
		default:
			s.logger.Warnf("unknown authorization header name %s", name)
			return nil
		}
	}

	if cookiesStr, ok := headers["cookie"]; ok {
		cookies, err := http.ParseCookie(cookiesStr)
		if err != nil {
			s.logger.Warnf("failed to parse cookies: %v", err)
			return nil
		}
		for _, cookie := range cookies {
			if cookie.Name == s.authCookieName {
				s.logger.Debugf("user authorization is %q cookie", s.authCookieName)
				return &ytsdk.CookieCredentials{Cookie: cookie}
			}
		}
	}

	s.logger.Warnf("no supported authorization method in headers: %q cookie, bearer/oauth token", s.authCookieName)
	return nil
}

var (
	okResponse = &authv3.CheckResponse{
		Status: &status.Status{
			Code: int32(codes.OK),
		},
		HttpResponse: &authv3.CheckResponse_OkResponse{
			OkResponse: &authv3.OkHttpResponse{},
		},
	}
	deniedResponse = &authv3.CheckResponse{
		Status: &status.Status{
			Code:    int32(codes.PermissionDenied),
			Message: "permission denied",
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
				Body:   "permission denied",
			},
		},
	}
)
