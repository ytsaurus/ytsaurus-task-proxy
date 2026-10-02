package pkg

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.ytsaurus.tech/yt/go/guid"
	"go.ytsaurus.tech/yt/go/yt"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

type AuthCacheConfig struct {
	Enabled                      bool
	TTLSeconds                   int
	Capacity                     int
	MaxConcurrentBackendRequests int
	RefreshBeforeSeconds         int
}

type authServer struct {
	authv3.UnimplementedAuthorizationServer

	mx                 sync.RWMutex
	hashToTasks        map[string]Task
	operationAliasToID map[string]string
	yt                 ytsdk.Client
	ytProxy            string
	logger             *SimpleLogger
	authCookieName     string
	cache              *authPermissionCache // cache is nil when caching is disabled.
}

func CreateAuthServer(yt ytsdk.Client, ytProxy string, logger *SimpleLogger, authCookieName string, cacheCfg AuthCacheConfig) *authServer {
	cache := newAuthPermissionCache(cacheCfg, logger)
	return &authServer{
		hashToTasks:    make(map[string]Task),
		mx:             sync.RWMutex{},
		yt:             yt,
		ytProxy:        ytProxy,
		logger:         logger,
		authCookieName: authCookieName,
		cache:          cache,
	}
}

func (s *authServer) Check(ctx context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	httpAttrs := req.GetAttributes().GetRequest().GetHttp()
	path := httpAttrs.GetPath()
	headers := httpAttrs.GetHeaders()
	host := httpAttrs.GetHost()

	task, err := s.findTaskByRequest(host, headers)
	if err != nil {
		defaultMetrics.ObserveAuthFailure(authReasonTaskLookup, nil)
		// Not an authz failure: the task isn't in our tables (yet). This is normal right
		// after an operation restart — the new operation must reach "running" and be
		// picked up by the next discovery pass. Return 503 (transient) rather than 403,
		// so the client retries instead of surfacing a scary "permission denied".
		s.logger.Warnf("task not found during auth check (likely starting): %s", err)
		return taskNotFoundResponse, nil
	}

	// skip auth for UI services for statics; currently it is the case for SPYT UI
	if task.service == "ui" && strings.HasPrefix(path, "/static") {
		s.logger.Debugf("skip auth for 'ui' service for statics on path %s", path)
		defaultMetrics.ObserveAuthSuccess(authReasonStaticBypass)
		return okResponse, nil
	}

	s.logger.Debugf("auth for path %q, task %v", path, task)

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

func (s *authServer) SetTasksData(hashToTasks map[string]Task, operationAliasToID map[string]string) {
	s.mx.Lock()
	defer s.mx.Unlock()

	s.hashToTasks = hashToTasks
	s.operationAliasToID = operationAliasToID
}

func (s *authServer) findTaskByRequest(host string, headers map[string]string) (*Task, error) {
	s.mx.RLock()
	defer s.mx.RUnlock()

	var hash string
	if routerHeaderValue, ok := headers[idRouterHeaderName]; ok {
		hash = routerHeaderValue
	} else if operationID, ok := headers[operationIDRouterHeaderName]; ok {
		hash = taskHash(operationID, headers[taskNameRouterHeaderName], headers[serviceRouteHeaderName])
	} else if operationAlias, ok := headers[operationAliasRouterHeaderName]; ok {
		operationID, ok := s.operationAliasToID[operationAlias]
		if !ok {
			return nil, fmt.Errorf("operation by alias %q from header was not found", operationAlias)
		}
		hash = taskHash(operationID, headers[taskNameRouterHeaderName], headers[serviceRouteHeaderName])
	} else if host != "" {
		subdomain := strings.Split(host, ".")[0]
		if operationAlias, taskName, service, ok := tryParseAliasSubdomain(subdomain); ok {
			operationID, ok := s.operationAliasToID[operationAlias]
			if !ok {
				return nil, fmt.Errorf("operation by alias %q from subdomain was not found", operationAlias)
			}
			hash = taskHash(operationID, taskName, service)
		} else {
			hash = subdomain
		}
	} else {
		return nil, fmt.Errorf("authority (host) or %s headers are missing in request", idRouterHeaderName)
	}

	if task, ok := s.hashToTasks[hash]; !ok {
		return nil, fmt.Errorf("no entry for hash %q in tasks registry", hash)
	} else {
		return &task, nil
	}
}

func (s *authServer) checkOperationPermission(ctx context.Context, operationID string, headers map[string]string) (bool, error) {
	userCredentials := s.getYTCredentialsFromHeaders(headers)
	if userCredentials == nil {
		s.logger.Warnf("request without credentials, headers: %v", headers)
		defaultMetrics.ObserveAuthFailure(authReasonCredentials, nil)
		return false, nil
	}

	cacheKey := authCacheKey{
		credentials: credentialsKey(userCredentials),
		operationID: operationID,
	}

	allowed, _, err := s.cache.GetOrLoad(ctx, cacheKey, func(checkCtx context.Context) (bool, string, error) {
		userYT, err := CreateYTClient(s.ytProxy, userCredentials, s.logger)
		if err != nil {
			defaultMetrics.ObserveAuthYTError("create_client", err)
			return false, "", err
		}

		whoAmIStarted := time.Now()
		userResp, err := userYT.WhoAmI(checkCtx, nil)
		defaultMetrics.ObserveYTDuration("whoami", time.Since(whoAmIStarted))
		if err != nil {
			s.logger.Errorf("whoami failed: dur=%s, err=%v", time.Since(whoAmIStarted), err)
			defaultMetrics.ObserveAuthYTError("whoami", err)
			return false, "", err
		}

		user := userResp.Login
		if user == "" {
			s.logger.Errorf("user not identified by provided credentials: %v", userResp)
			defaultMetrics.ObserveAuthFailure(authReasonUserNotIdentified, nil)
			return false, "", nil
		}
		s.logger.Debugf("auth user is %q", user)

		operationIDg, err := guid.ParseString(operationID)
		if err != nil {
			s.logger.Warnf("invalid operation ID %s", operationID)
			defaultMetrics.ObserveAuthFailure(authReasonInvalidOperation, nil)
			return false, "", nil
		}

		permissionCheckStarted := time.Now()
		resp, err := s.yt.CheckOperationPermission(
			checkCtx,
			yt.OperationID(operationIDg),
			user,
			yt.PermissionRead,
			nil,
		)
		defaultMetrics.ObserveYTDuration("check_operation_permission", time.Since(permissionCheckStarted))
		if err != nil {
			s.logger.Infof("permission check failed: dur=%s, err=%v", time.Since(permissionCheckStarted), err)
			defaultMetrics.ObserveAuthYTError("permission_check", err)
			return false, "", err
		}

		allowed := resp.Action == "allow"
		s.logger.Debugf("check operation permission result is %q for user %q and operation %q", resp.Action, user, operationID)
		return allowed, user, nil
	})
	if err != nil {
		return false, err
	}

	if !allowed {
		defaultMetrics.ObserveAuthFailure(authReasonPermissionDenied, nil)
		return false, nil
	}
	defaultMetrics.ObserveAuthSuccess(authReasonAuthorized)
	return true, nil
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
	// Returned when the requested task is not (yet) in the routing tables — typically a
	// notebook/clique whose operation just restarted and hasn't been rediscovered.
	// Transient, so 503 (not 403): the client should retry, and it isn't an authz failure.
	taskNotFoundResponse = &authv3.CheckResponse{
		Status: &status.Status{
			Code:    int32(codes.Unavailable),
			Message: "task not found or starting",
		},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{
			DeniedResponse: &authv3.DeniedHttpResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode_ServiceUnavailable},
				Body:   "task not found or starting, please retry",
			},
		},
	}
)

func taskHash(operationID, taskName, service string) string {
	return (&Task{operationID: operationID, taskName: taskName, service: service}).Hash()
}
