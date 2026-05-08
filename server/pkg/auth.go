package pkg

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.ytsaurus.tech/yt/go/guid"
	"go.ytsaurus.tech/yt/go/yt"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

type AuthCacheConfig struct {
	Enabled    bool
	TTLSeconds int
	Capacity   int
}

type authCacheKey struct {
	credentials string
	operationID string
}

func credentialsKey(creds ytsdk.Credentials) string {
	if creds == nil {
		return ""
	}
	switch v := creds.(type) {
	case *ytsdk.TokenCredentials:
		return "token:" + v.Token
	case *ytsdk.BearerCredentials:
		return "bearer:" + v.Token
	case *ytsdk.CookieCredentials:
		return "cookie:" + v.Cookie.Value
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
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
	cache              *lru.LRU[authCacheKey, bool] // cache is nil when caching is disabled.
}

func CreateAuthServer(yt ytsdk.Client, ytProxy string, logger *SimpleLogger, authCookieName string, cacheCfg AuthCacheConfig) *authServer {
	var cache *lru.LRU[authCacheKey, bool]
	if cacheCfg.Enabled {
		ttl := time.Duration(cacheCfg.TTLSeconds) * time.Second
		cache = lru.NewLRU[authCacheKey, bool](cacheCfg.Capacity, nil, ttl)
	}
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
		s.logger.Warnf("failed to find task during auth check: %s", err)
		return deniedResponse, nil
	}

	// skip auth for UI services for statics; currently it is the case for SPYT UI
	if task.service == "ui" && strings.HasPrefix(path, "/static") {
		s.logger.Debugf("skip auth for 'ui' service for statics on path %s", path)
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
		return false, nil
	}

	cacheKey := authCacheKey{
		credentials: credentialsKey(userCredentials),
		operationID: operationID,
	}
	if s.cache != nil {
		if allowed, ok := s.cache.Get(cacheKey); ok {
			s.logger.Debugf("cache hit for operation %q: allowed=%v", operationID, allowed)
			return allowed, nil
		}
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

	allowed := resp.Action == "allow"
	s.logger.Debugf("check operation permission result is %q for user %q and operation %q", resp.Action, user, operationID)

	if s.cache != nil {
		s.cache.Add(cacheKey, allowed)
	}

	return allowed, nil
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

func taskHash(operationID, taskName, service string) string {
	return (&Task{operationID: operationID, taskName: taskName, service: service}).Hash()
}
