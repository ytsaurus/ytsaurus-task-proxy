package pkg

import (
	"testing"

	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestAuthResponsesDifferentiatePermissionDeniedAndInfrastructure(t *testing.T) {
	require.Equal(t, int32(codes.PermissionDenied), deniedResponse.GetStatus().GetCode())
	require.Equal(t, typev3.StatusCode_Forbidden, deniedResponse.GetDeniedResponse().GetStatus().GetCode())
	require.Equal(t, "permission denied", deniedResponse.GetDeniedResponse().GetBody())

	require.Equal(t, int32(codes.Unavailable), unavailableResponse.GetStatus().GetCode())
	require.Equal(t, typev3.StatusCode_ServiceUnavailable, unavailableResponse.GetDeniedResponse().GetStatus().GetCode())
	require.Equal(t, "authorization backend unavailable", unavailableResponse.GetDeniedResponse().GetBody())
}
