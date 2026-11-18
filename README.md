# YTsaurus Task proxy

YTsaurus operations often require deploying web services. These can be debugging UIs (such as Spark UI in [SPYT](https://ytsaurus.tech/docs/en/user-guide/data-processing/spyt/overview)), ML model inference servers, or APIs inside jobs.

Operation jobs run on cluster exec nodes, so services bind to network ports on these nodes — to receive incoming traffic. However, when attempting to directly access services inside a job, difficulties arise:

- Network isolation: the user may not have direct network access to exec node IP addresses (they may be in a closed perimeter).
- Dynamic addressing: even with network access, jobs can move between nodes, so the host and port of services constantly change.
- Security: direct access to a port on a node bypasses YTsaurus authentication mechanisms. Access control to the operation is not enforced.

_Task proxy_ solves these problems by providing a single entry point. It allocates stable domains (FQDN) for each service and verifies user access rights before redirecting the request inside the job.

For more information, refer to:
- [User docs](https://ytsaurus.tech/docs/user-guide/proxy/task) for usage examples,
- [Spark UI](https://ytsaurus.tech/docs/user-guide/data-processing/spyt/spark-ui) to learn how to open UI of [SPYT](https://ytsaurus.tech/docs/en/user-guide/data-processing/spyt/overview) clusters and jobs,
- [Admin docs](https://ytsaurus.tech/docs/admin-guide/install-task-proxy) for installation instructions.

## Annotating an operation

To publish services from a regular YTsaurus operation, add the `task_proxy` annotation to its specification. `enabled` is required; `tasks_info` describes services by task name, service name, protocol, and zero-based job port index.

```yson
<"task_proxy"={
    "enabled"=%true;
    "tasks_info"={
        "worker"={
            "api"={
                "protocol"="http";
                "port_index"=0;
            };
            "grpc"={
                "protocol"="grpc";
                "port_index"=1;
            };
        };
    };
}>
```

`protocol` must be `http`, `grpc`, or `websocket`. A `websocket` service is proxied like `http`, but Envoy additionally accepts the `Upgrade: websocket` handshake on its routes; after the upgrade the connection is tunneled to the job. Note that `stream_idle_timeout_seconds` still applies to an idle WebSocket connection, so raise it or set it to `0` for connections that stay silent for a long time. If `tasks_info` is omitted, task-proxy publishes every job port as an HTTP service named `port<N>`.

The annotation can also override request timeouts for every service in that operation:

```yson
<"task_proxy"={
    "enabled"=%true;
    "route_timeout_seconds"=600;
    "stream_idle_timeout_seconds"=120;
}>
```

- `route_timeout_seconds` is the maximum time to receive a complete upstream response after Envoy has received the full request.
- `stream_idle_timeout_seconds` is the maximum period without request or response traffic.
- Both values are non-negative integer seconds. `0` explicitly disables the corresponding timeout; an omitted value inherits the global Helm setting.

The Helm defaults are 2 seconds for connecting to a job, 15 seconds for a complete response, and 300 seconds for a stream idle period. Configure them through `timeouts.connectTimeoutSeconds`, `timeouts.routeTimeoutSeconds`, and `timeouts.streamIdleTimeoutSeconds`.

## Authorization and YT request timeouts

Two independent settings control authorization and calls to YT:

| Helm value | Server flag | Default | Scope |
| --- | --- | --- | --- |
| `timeouts.authTimeout` | `-auth-timeout` | `800ms` | Complete Envoy external authorization check (`ext_authz.grpc_service.timeout`). |
| `timeouts.ytLightRequestTimeout` | `-yt-light-request-timeout` | `10s` | One light YT SDK call, including retries and backoffs. Applied to the service client (discovery, permission checks, registry writes) and user clients created for `WhoAmI`. |

Use positive Go duration strings with explicit units, such as `800ms`, `2.5s`, or `1m`. Zero, negative, malformed and overflowing values are rejected at startup, even if authorization is disabled. Existing route timeouts and their operation annotation overrides remain separate.

On a cache miss, authorization normally runs `WhoAmI` and `CheckOperationPermission` sequentially. The outer authorization budget must cover both calls, their retries/backoffs and network overhead. `ytLightRequestTimeout` limits each individual SDK call; an earlier deadline from the outer request can interrupt it sooner. Choose the budgets together: increasing only the YT timeout cannot extend Envoy's authorization deadline. This does not configure cancellation of work on the YT HTTP proxy after a client disconnects. See also the [Envoy ext_authz configuration](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_authz_filter).

For example, add to your deployment values file:

```yaml
timeouts:
  authTimeout: "25s"
  ytLightRequestTimeout: "10s"
```

These are illustrative values, not a recommendation for a particular cluster. With a chart and server image containing support for these settings, subsequent changes only require updating the deployment configuration and restarting the pods; no image rebuild is needed. There is no hot reload.

For a Helm-managed installation, apply your complete values file through the normal release workflow, for example:

```sh
helm upgrade task-proxy ./chart -n "$NAMESPACE" -f values.yaml
kubectl rollout status deployment/task-proxy -n "$NAMESPACE"
```

For a Terraform-managed release, change its Helm values in Terraform and use the normal plan/apply workflow. For a standalone binary, pass `-auth-timeout=25s -yt-light-request-timeout=10s` and restart it.

Verify each server replica's startup log contains the selected values:

```text
authorization timeouts: auth-timeout=25s, yt-light-request-timeout=10s
```

Independently inspect the **active** configuration on each Envoy replica. Forward its admin port (replace `POD` with one pod's name):

```sh
kubectl port-forward -n "$NAMESPACE" pod/POD 9901:9901
```

In another terminal, read the timeout actually accepted from xDS:

```sh
curl -fsS http://127.0.0.1:9901/config_dump | jq -r '
  .configs[]
  | select(."@type" == "type.googleapis.com/envoy.admin.v3.ListenersConfigDump")
  | .dynamic_listeners[].active_state.listener.filter_chains[].filters[]
  | .typed_config.http_filters[]
  | select(.name == "envoy.filters.http.ext_authz")
  | .typed_config.grpc_service.timeout'
```

The example should report `25s`. The filter is absent when `auth.enabled` is false. The YT SDK timeout is not an Envoy field: check server arguments/logs and use a controlled delayed YT response to verify its deadline. In a staging environment, verify an uncached authorization response later than 800ms succeeds within the new budget, a response beyond that budget fails, and a successful check fills the cache.

## Development

Install chart to cluster from local directory using:

```sh
helm install task-proxy \
    -n ${NAMESPACE} \
    -f values.yaml \
    ./chart
```

Run all Go tests (including startup validation and delayed HTTP checks for both YT clients) with `make test`. The optional Envoy integration test requires Docker Desktop or Colima with `host.docker.internal` support and a local Envoy image:

```sh
docker pull envoyproxy/envoy:v1.36-latest
cd server
TASK_PROXY_ENVOY_IMAGE=envoyproxy/envoy:v1.36-latest \
  go test ./pkg -run '^TestEnvoyAuthorizationTimeout$' -v -count=1
```

It publishes generated snapshots over xDS, reads back the active timeout from Envoy's `config_dump`, and exercises the real authorization handler against a delayed local YT stub. It verifies the old 800ms deadline, acceptance of a 1s response with a 2s budget, cache population, and failure after the new deadline. It creates and removes its own local container and does not contact a YT cluster.
