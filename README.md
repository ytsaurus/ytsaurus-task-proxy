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

`protocol` must be `http` or `grpc`. If `tasks_info` is omitted, task-proxy publishes every job port as an HTTP service named `port<N>`.

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

## Development

Install chart to cluster from local directory using:

```sh
helm install task-proxy \
    -n ${NAMESPACE} \
    -f values.yaml \
    ./chart
```
