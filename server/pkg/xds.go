package pkg

import (
	"fmt"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	accesslog3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	accesslogstream3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/stream/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	clustergrpc "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	listenergrpc "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

const (
	extAuthClusterName = "extAuthz"
	routerHeaderName   = "x-yt-taskproxy-id"
)

func ServeGRPC(s serverv3.Server, authServer *authServer) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", serverPort))
	if err != nil {
		return err
	}
	gs := grpc.NewServer()

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(gs, s)
	clustergrpc.RegisterClusterDiscoveryServiceServer(gs, s)
	listenergrpc.RegisterListenerDiscoveryServiceServer(gs, s)

	authv3.RegisterAuthorizationServer(gs, authServer)

	log.Printf("xDS + extAuthz starts listening on :%d", serverPort)

	return gs.Serve(lis)
}

func makeSnapshot(hashToTask map[string]Task, version string, baseDomain string, tls bool, authEnabled bool) (*cachev3.Snapshot, error) {
	var clusters []cachetypes.Resource
	var vhosts []*routev3.VirtualHost

	var defaultVhostRoutes []*routev3.Route

	for hash, task := range hashToTask {
		grpc := task.protocol == "grpc"
		vhostName := fmt.Sprintf("%s-%s-%s", task.operationID, task.taskName, task.service)

		var vhostClusters []*routev3.WeightedCluster_ClusterWeight
		for i, job := range task.jobs {
			clusterName := fmt.Sprintf("%s-%d", vhostName, i)
			clusters = append(clusters, makeCluster(clusterName, job.host, job.port, grpc, true))
			vhostClusters = append(vhostClusters, &routev3.WeightedCluster_ClusterWeight{
				Name:   clusterName,
				Weight: &wrapperspb.UInt32Value{Value: 1},
			})
		}
		action := &routev3.Route_Route{
			Route: &routev3.RouteAction{
				ClusterSpecifier: &routev3.RouteAction_WeightedClusters{
					WeightedClusters: &routev3.WeightedCluster{
						Clusters: vhostClusters,
					},
				},
			},
		}
		// route either by domain
		vhosts = append(vhosts, &routev3.VirtualHost{
			Name:    vhostName,
			Domains: []string{getTaskDomain(hash, baseDomain)},
			Routes: []*routev3.Route{{
				Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: action,
			}},
		})
		// ... or by custom header
		defaultVhostRoutes = append(defaultVhostRoutes, &routev3.Route{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
				Headers: []*routev3.HeaderMatcher{
					{
						Name: routerHeaderName,
						HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
							StringMatch: &matcherv3.StringMatcher{
								MatchPattern: &matcherv3.StringMatcher_Exact{
									Exact: hash,
								},
							},
						},
					},
				},
			},
			Action: action,
		})
	}

	defaultVhostRoutes = append(defaultVhostRoutes, &routev3.Route{
		Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_DirectResponse{
			DirectResponse: &routev3.DirectResponseAction{
				Status: 404,
				Body: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineString{InlineString: "no such task"},
				},
			},
		},
	})
	vhosts = append(vhosts, &routev3.VirtualHost{
		Name:    "vhost_default",
		Domains: []string{"*"},
		Routes:  defaultVhostRoutes,
	})

	if authEnabled {
		authzCluster := makeCluster(extAuthClusterName, "127.0.0.1", serverPort, true, false)
		clusters = append(clusters, authzCluster)
	}

	// HTTP filters: ext_authz before router
	authz := &extauthzv3.ExtAuthz{
		Services: &extauthzv3.ExtAuthz_GrpcService{
			GrpcService: &corev3.GrpcService{
				TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
						ClusterName: extAuthClusterName,
					},
				},
				Timeout: durationpb.New(800 * time.Millisecond),
			},
		},
		FailureModeAllow:       false,
		IncludePeerCertificate: false,
	}

	var httpFilters []*hcmv3.HttpFilter
	if authEnabled {
		httpFilters = append(httpFilters, &hcmv3.HttpFilter{
			Name: "envoy.filters.http.ext_authz",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{
				TypedConfig: mustAny(authz),
			},
		})
	}
	httpFilters = append(httpFilters, &hcmv3.HttpFilter{
		Name: "envoy.filters.http.router",
		ConfigType: &hcmv3.HttpFilter_TypedConfig{
			TypedConfig: mustAny(&routerv3.Router{}),
		},
	})

	// HCM using RDS via ADS
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: "ingress_http",
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{
			RouteConfig: &routev3.RouteConfiguration{
				Name:         "local_routes",
				VirtualHosts: vhosts,
			},
		},
		CodecType:            hcmv3.HttpConnectionManager_AUTO,
		HttpFilters:          httpFilters,
		Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
	}

	var transportSocket *corev3.TransportSocket
	if tls {
		transportSocket = &corev3.TransportSocket{
			Name: "envoy.transport_sockets.tls",
			ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: mustAny(
				&tlsv3.DownstreamTlsContext{
					CommonTlsContext: &tlsv3.CommonTlsContext{
						TlsCertificates: []*tlsv3.TlsCertificate{{
							CertificateChain: &corev3.DataSource{
								Specifier: &corev3.DataSource_Filename{
									Filename: TLSCrtPath,
								},
							},
							PrivateKey: &corev3.DataSource{
								Specifier: &corev3.DataSource_Filename{
									Filename: TLSKeyPath,
								},
							},
						}},
					},
				},
			)},
		}
	}

	listener := &listenerv3.Listener{
		Name: "listener_0",
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: proxyPort,
					},
				},
			},
		},
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(hcm)},
			}},
			TransportSocket: transportSocket,
		}},
		AccessLog: []*accesslog3.AccessLog{
			{
				Name: "envoy.access_loggers.stderr",
				ConfigType: &accesslog3.AccessLog_TypedConfig{
					TypedConfig: mustAny(&accesslogstream3.StderrAccessLog{
						/* AccessLogFormat: &accesslogstream3.StderrAccessLog_LogFormat{
							LogFormat: &corev3.SubstitutionFormatString{
								Format: &corev3.SubstitutionFormatString_TextFormat{
									TextFormat: "%LOCAL_REPLY_BODY%:%RESPONSE_CODE%:path=%REQ(:path)%\n",
								},
							},
						}, */
					}),
				},
			},
		},
	}

	snap, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]cachetypes.Resource{
		resourcev3.ClusterType:  clusters,
		resourcev3.ListenerType: {listener},
	})
	if err != nil {
		return nil, err
	}
	return snap, snap.Consistent()
}

func makeCluster(name string, host string, port uint32, grpc bool, resolveDomain bool) *clusterv3.Cluster {
	discoveryType := clusterv3.Cluster_STATIC
	if resolveDomain {
		discoveryType = clusterv3.Cluster_STRICT_DNS
	}

	cluster := clusterv3.Cluster{
		Name:                 name,
		ConnectTimeout:       durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: discoveryType},
		LbPolicy:             clusterv3.Cluster_ROUND_ROBIN,
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Protocol: corev3.SocketAddress_TCP,
										Address:  host,
										PortSpecifier: &corev3.SocketAddress_PortValue{
											PortValue: port,
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
	}
	if grpc {
		cluster.TypedExtensionProtocolOptions = map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				&httpv3.HttpProtocolOptions{
					UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
						ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
							ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
								Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
							},
						},
					},
				},
			),
		}
	}
	return &cluster
}

func mustAny(m proto.Message) *anypb.Any {
	a, err := anypb.New(m)
	if err != nil {
		panic(err)
	}
	return a
}
