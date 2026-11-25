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

## Monitoring requests by user

Task Proxy exposes `yt_task_proxy_user_requests_total{user,cache,auth_result}` on the
server's `/metrics/prometheus` endpoint. With `monitoring.enabled: true`, the
Helm chart scrapes server and Envoy metrics through the internal
`<release>-metrics` service. The `job` label keeps the release name and the
`service` label uses the new service name. Update custom scrape targets and
queries that filter on the old service name.

The counter increments once for each authorization request with a verified
YTsaurus login, including requests denied by the operation permission check.
Requests that bypass authorization or fail before a login is known are excluded.
For gRPC streams and WebSockets, the counter measures authorization of the
request or handshake.

- `user` is the verified YTsaurus login. Restrict access to the metrics endpoint
  and the monitoring backend.
- `cache` describes the operation permission cache. It is `hit` when a valid
  entry is available on the initial lookup, `miss` when there is no valid
  entry on that lookup (including requests waiting for an in-flight check),
  or `disabled` when the cache is off.
- `auth_result` is the final authorization outcome. The current outcomes are
  `allowed`, `permission_denied` for an operation ACL denial,
  `invalid_operation_id`, and the existing `infra_*` failure reasons such as
  `infra_context_deadline_exceeded` and `infra_grpc_unavailable`. A cached
  decision retains its outcome. Background cache refreshes are excluded from
  this counter.

Each replica exports one series per observed combination of labels. When rate
limiting is added, a rejected request can use `auth_result="rate_limited"` in
the same counter. The `cache` label will still describe the ACL lookup.

Use these PromQL expressions for a single user. Filter by deployment labels
such as `namespace` and `job` when sharing a monitoring backend.

```promql
# Graph the user's average RPS over the last 5 minutes.
sum by (user) (rate(yt_task_proxy_user_requests_total{user="alice"}[5m]))

# Estimate the user's total requests over the last hour.
sum by (user) (increase(yt_task_proxy_user_requests_total{user="alice"}[1h]))

# Break down the user's requests by cache status over the last hour.
sum by (cache) (increase(yt_task_proxy_user_requests_total{user="alice"}[1h]))

# Break down the user's denied and failed requests by cache status and result.
sum by (cache, auth_result) (increase(yt_task_proxy_user_requests_total{user="alice",auth_result!="allowed"}[1h]))
```

Calculate `rate` or `increase` for each series before summing across replicas
so that Prometheus can account for counter resets. `increase` extrapolates
between scrapes and may return a fractional count. A newly observed series
needs at least two samples for these queries.

Example Prometheus alerting rule for sustained load from one user:

```yaml
groups:
  - name: task-proxy-user-requests
    rules:
      - alert: TaskProxyHighUserRPS
        expr: sum by (user) (rate(yt_task_proxy_user_requests_total{user="alice"}[5m])) > 100
        for: 10m
        annotations:
          summary: "Task Proxy request rate for {{ $labels.user }} exceeds 100 RPS"
```

## Development

Install chart to cluster from local directory using:

```sh
helm install task-proxy \
    -n ${NAMESPACE} \
    -f values.yaml \
    ./chart
```
