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

## Development

Install chart to cluster from local directory using:

```sh
helm install task-proxy \
    -n ${NAMESPACE} \
    -f values.yaml \
    ./chart
```

## Logging and retained storage

Server logs, Envoy system logs and Envoy listener access logs are configured independently. The defaults are server `stderr`/`debug`, Envoy `stderr`/`info`, and enabled access logging to `stderr`. Supported writers are `stdout`, `stderr`, and `file`; severities are `trace`, `debug`, `info`, `warning`, and `error`. Access logging has an `enabled` switch and no severity setting. It retains the existing listener placement: it describes listener events, including rejected connections, and does not enable HTTP request access logging.

For example, use file output for all three streams:

```yaml
replicas: 2
server:
  logging:
    writerType: file
    minLogLevel: debug
proxy:
  logging:
    writerType: file
    minLogLevel: info
  accessLog:
    enabled: true
    writerType: file
persistence:
  storageClass: null
  size: 5Gi
  accessModes: [ReadWriteOnce]
```

Each StatefulSet replica gets its own PVC, named `logs-<release>-<ordinal>`, mounted directly at `/var/log/task-proxy` in both containers. Active filenames are fixed: `server.log`, `envoy.log`, and `access.log`. There are no per-Pod directories or shared claim option. Claims are allocated even in console mode, so toggling file output does not change immutable StatefulSet claim templates. A default StorageClass is needed when `storageClass` is `null`; an explicit empty string requests no storage class. The default `podSecurityContext.fsGroup: 101` makes the mount writable to the upstream Envoy entrypoint's UID/GID 101, provided the volume driver supports group ownership. If changing this setting or the image, ensure Envoy actually runs with the matching writable GID; the upstream entrypoint can drop supplemental groups.

File writers without a custom `rotationPolicy` use:

```yaml
rotationPolicy:
  rotationPeriodMilliseconds: 3600000
  maxSegmentSize: 100Mi
  maxTotalSizeToKeep: 1Gi
  maxSegmentCountToKeep: 10
```

A nonempty custom policy replaces these defaults, and must include at least one rotation trigger (period or segment size) and one retention bound (archive count or total archive size). Limits supplied explicitly must be positive. Byte quantities accept integer bytes or decimal/binary units, such as `100MB`, `100Mi`, or `1Gi`. A minimal size-and-count policy is `maxSegmentSize: 8Mi` plus `maxSegmentCountToKeep: 3`. Helm validates policy shape and positive quantity syntax; the strict server loader additionally rejects quantities that resolve to fractional bytes or exceed int64. An unusually large or fractional custom value can therefore render but be rejected at server startup. Console writers must omit rotation policies; YAML `null` or an empty policy in Helm values is normalized to no policy for console output and the defaults for file output. Disabling access logging suppresses its sink and rotation.

The server checks rotation once per second. It renames whole active files into timestamped archives and reopens them; it does not truncate or copy records. Envoy files reopen through the local admin endpoint. On admin failure or delayed file recreation, the archive is preserved and reopen is retried. The latest external archive remains protected until a subsequent successful reopen, so buffered data is not removed. Retention limits apply only to archives and separately to each stream. Active files are excluded and consume additional space; thresholds are soft and can be exceeded between checks or while reopen/storage errors persist. Size claims for all enabled streams and operational headroom: the default 5Gi claim accommodates the three nominal 1Gi archive budgets and active segments, but is not a hard aggregate log limit. Logging/storage failures are reported to stderr. Rotation recognizes only its own regular archive filenames and preserves unrelated files. Restart continues existing active files and archives; the initial age of an existing active file uses its modification time, rather than recovering its original creation time.

The logging ConfigMap checksum triggers Pod replacement after configuration changes. PVCs are explicitly retained after StatefulSet deletion or scale-down; the same release name and ordinal reattach the history on replacement or scale-up. Retained claims consume storage until an administrator removes them. Retention of claims does not replace storage backups or change a StorageClass reclaim policy. Changing `persistence.size`, `storageClass`, or `accessModes` changes immutable claim templates and requires a separately planned storage migration; ordinary logging-mode and severity changes leave them unchanged.

### Migrating an existing Deployment

This chart now creates a StatefulSet in every logging mode. Before upgrading an installation of the previous Deployment chart, save the current Helm values and explicitly stop the old Deployment (for example, scale it to zero). Then upgrade using the preserved values and the chosen persistence settings. Schedule the resulting downtime: running both controllers concurrently can route traffic to both versions. Verify the new StatefulSet, its individually bound claims, and routing after upgrading. Helm normally removes the old workload as part of the chart upgrade; if an obsolete Deployment remains, remove it only after verification. Do not reuse a shared existing claim for the new replicas. Previous console logs remain in the cluster's logging system; the migration cannot manufacture durable history from them. Plan rollback of the workload kind separately and retain the new claims until their history is no longer needed.

### Standalone process

Pass `-logging-config=/path/to/logging.yaml` to the server. Its strict YAML format uses `server`, `proxy`, and `accessLog` sections (the contents of the Helm sections above) and optionally an absolute `directory` (default `/var/log/task-proxy`). Partial files inherit console defaults; unknown fields, null configuration fields, and multiple YAML documents are rejected. Standalone file writers must supply their rotation policy explicitly; Helm supplies file defaults during rendering. Configure Envoy's `--log-level` and, for file/stdout output, `--log-path` consistently with the server config. Default stderr output omits `--log-path`. File output uses `<directory>/envoy.log`; stdout uses `/dev/stdout`. Envoy's admin endpoint must be reachable by the server at `127.0.0.1:9901`. Prepare a directory writable by both processes before starting Envoy.

### Verification

`make test` runs all Go packages, including the main package and Helm render tests; install Helm 3 first. `make test-chart` additionally runs `helm lint`. Chart tests parse the rendered logging YAML through the real strict runtime loader and check replica claims, permissions, mounts, console/file flags, disabled access output, TLS, invalid policies, and configuration checksums.

`make test-integration` requires Docker, a free local port 9901, and the Envoy image (`TASK_PROXY_ENVOY_IMAGE` can override `envoyproxy/envoy:v1.36-latest`). It runs real Envoy against the generated xDS resources in a static bootstrap and uses the production logging backend for rename/admin reopen and retention. A retained bind mount verifies replacement appending and archive preservation; test fixtures use self-ignored temporary folders under `.superpowers/` because Colima does not share host `/tmp`. This test does not validate Kubernetes CSI ownership handling, PVC provisioning/rebinding, StatefulSet rollout, or Deployment migration; those require a separate nonproduction Kubernetes check. CI runs the render tests on Linux/macOS and the Docker integration on Linux.
