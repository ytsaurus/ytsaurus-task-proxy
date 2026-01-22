# Task proxy

YTsaurus operations can launch some web services in their jobs and users may want to access such services. I.e., [SPYT](https://ytsaurus.tech/docs/en/user-guide/data-processing/spyt/overview) operations launch web UI services so users can access Spark UI. User may deploy some model inference server in operation with gRPC. And so on.

Jobs are executed on exec nodes, so services are binded to exec nodes on some ports. But there is some issues when accessing exec nodes directly:
- Users may not have direct access to exec nodes.
- Even if users have access to exec nodes, jobs can move between nodes so services (host, port)-s are constantly changing.
- There is no operation access control while accessing exec nodes.

To address these issues, _task proxy_ component was developed. Task proxy provides FQDNs for each service and checks operation access with [`check_operation_persmission`](https://ytsaurus.tech/docs/en/api/commands#check_operation_permission) API method before proxying user request to job.

## Installation

```sh
helm install task-proxy \                
    -n ${NAMESPACE} \                                
    -f values.yaml \
    oci://ghcr.io/ytsaurus/task-proxy-chart --version ${VERSION}
```

Example of `values.yaml`:
```yaml
# Path to table where tasks info will be stored
tablePath: //home/task_proxies

# Services domains will use this domain as base, adding service hash, 
# i.e. 645236d8.my-cluster.ytsaurus.example.net
baseDomain: my-cluster.ytsaurus.example.net

# k8s secret name with task proxy user token, see details of how to create such user below
tokenSecretRef: task-proxy-token

# Task proxy consists of two apps, collocated in single pod:
# - Proxy (https://www.envoyproxy.io/), which implements proxying to jobs and auth handling
# - Server, which discovers services and provides Envoy its configuration with xDS protocol
# In these section you can specify replicas count and resources for each app.
replicas: 2
proxy:
  resources:
    requests:
      cpu: "1"
      memory: 1Gi
server:
  resources:
    requests:
      cpu: "1"
      memory: 1Gi

# k8s nodes selector (specify your labels) and pods anti-affinity 
# to prevent allocation of pods on same k8s node
nodeSelector:
  yt-group: tp
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
    - labelSelector:
        matchExpressions:
        - key: app.kubernetes.io/component
          operator: In
          values:
          - task-proxy
      topologyKey: kubernetes.io/hostname
```

Create task proxy user and k8s secret with its token:
```sh
yt create user --attr "{name=robot-task-proxy}"
yt issue-token robot-task-proxy > token
yt set //sys/operations/@acl/end '{subjects=[robot-task-proxy];permissions=[read];action=allow}'
yt set //home/@acl/end '{action=allow; subjects=[robot-task-proxy]; permissions=[read; write; create; remove;]}'
yt set //sys/accounts/sys/@acl/end '{action=allow; subjects=[robot-task-proxy]; permissions=[use]}'
kubectl create secret generic task-proxy-token -n yt --from-file token
```

## How it works

The most important part of task proxy is services discovery mechanism, implemented in server. It lists running operations and searches operations with web services which can be accessed by users. Then it generates domain for each service, using `baseDomain` and adding hash from service descriptor. This domains are used to access services.

Each service is described by triple _(operationID, taskName, service)_, which means:
- _Operation_ as primary unit of YTsaurus clusters workload.
- Operation consists of _tasks_, each task has its name.
- Each task can expose multiple web _services_, i.e. Spark master task expose HTTP services for UI and rest API.
- Each task can be launched in multiple instances, _jobs_, task proxy balance traffic between jobs.

To run operation with services, discoverable by task proxy, you should do the following:
- Specify `port_count` parameter in task spec, so YTsaurus dynamically allocate available ports for jobs on exec nodes. Ports will be available in environment variables `YT_PORT_0`, `YT_PORT_1`, etc., depending on number of requested ports from `port_count`.
- Specify `task_proxy={enabled=%true}` in operation annotation.

In this example we run sample HTTP server using single port (`port_count=1`) with two instances (`job_count=2`):
```sh
yt vanilla \
    --tasks '{example_http_server={job_count=2; command="python3 -m http.server ${YT_PORT_0}"; port_count=1}}' \
    --spec '{annotations={task_proxy={enabled=%true;}}}'
```

Task proxy server discovers such operations and write their services data in table, which path is specified in Helm chart's values by `tablePath` parameter. Here is the example of data in table, each row describes single service:

| __domain__                               | __operation_id__                  |  __task_name__      | __service__ | __protocol__ |
|------------------------------------------|-----------------------------------|---------------------|-------------|--------------|
| 645236d8.my-cluster.ytsaurus.example.net | a6e04b98-bf982394-5103e8-55754a49 | example_http_server | port_0      | http         |
| ae5cf6f5.my-cluster.ytsaurus.example.net | a8ef7695-3de07913-5103e8-e29a6707 | example_grpc_server | server      | grpc         |
| 2ef4261c.my-cluster.ytsaurus.example.net | a6e04b98-bf982394-5103e8-55754a49 | master              | ui          | http         |
| 51a6d485.my-cluster.ytsaurus.example.net | a6e04b98-bf982394-5103e8-55754a49 | history             | ui          | http         |
| 37a5f11c.my-cluster.ytsaurus.example.net | 6699a5a9-37c731e3-5103e8-b05d7dd0 | driver              | ui          | http         |     

- First row represents our [vanilla](https://ytsaurus.tech/docs/en/user-guide/data-processing/operations/vanilla) operation with sample HTTP server.
- Seconds row describes [sample](examples/grpc-service/) gRPC server, example of how to launch it, using extended task proxy annotation format is below.
- Third and fourth rows refer to [standalone](https://ytsaurus.tech/docs/en/user-guide/data-processing/spyt/cluster/cluster-start) SPYT cluster, launched with history server.
- Fiths row represents SPYT [direct submit](https://ytsaurus.tech/docs/en/user-guide/data-processing/spyt/direct-submit/desc).

SPYT operations, for both standalone clusters and direct submits, are discovered automatically.

You can make requests to this HTTP server in two ways.

Make request directly to task proxy, using its k8s service endpoint:
```sh
curl \
  -H "Host: 645236d8.my-cluster.ytsaurus.example.net" \
  -H "Authorization: OAuth ${YT_TOKEN}" \
  "http://task-proxy.${NAMESPACE}.svc.cluster.local:80"
```
You have to specify service domain in `Host` header so task proxy can route you request to corresponding jobs. Auth can be made by Cypress token, IAM token or auth cookie. We use Cypress token in this example.

If you have some ingress controller, you can make requests over Internet, using service domain directly. Tuning DNS records and TLS are in your responsibility. Example of how to tune [Gwin](https://yandex.cloud/en/docs/application-load-balancer/tools/gwin) controller to provide such access is below.
```sh
curl \
  -H "Authorization: Bearer ${IAM_TOKEN}" \
  "https://645236d8.my-cluster.ytsaurus.example.net"
```

To open SPYT UI, just paste service domain in your browser. If you have correct cookies in your `baseDomain`, task proxy will use it to auth your browser request. Auth cookie name is specified in Helm chart values in `auth.cookieName` parameter.

## Extended task proxy annotation format

Annotation `task_proxy={enabled=%true}` is minimal required format. Using it, each service given port indexes in order of task appearance and default service names `port_${PORT_INDEX}`, as well as `HTTP` protocol. If you want to specify service names, which port index they use and protocol, you can use extended annotation format.

Here is and example of launching operation with extended format for sample gRPC server. You can study [example](examples/grpc-service/) of how to build such server.

```sh
yt vanilla \
    --tasks '{example_grpc_server={job_count=2; command="./yt-sample-grpc-service"; file_paths = ["//home/yt-sample-grpc-service"]; port_count=1}}' \
    --spec '{annotations={task_proxy={enabled=%true; tasks_info={example_grpc_server={server={protocol="grpc"; port_index=0}}}}}}'
```

In `tasks_info` we provide each task with its attributes. Our operation has single task `example_grpc_server`, and its attributes are:
- `server` – single service of `example_grpc_server` task
  - `protocol="grpc"` specifies that we using gRPC, not HTTP protocol
  - `port_index=0` specifies that we using first port from `port_count` range.

Requests to sample gRPC server can be made as following.

Directly to task proxy:
```sh
grpcurl \
  -plaintext \
  -authority "ae5cf6f5.my-cluster.ytsaurus.example.net" \
  -H "Authorization: Bearer ${IAM_TOKEN}" \
  "task-proxy.${NAMESPACE}.svc.cluster.local:80" \
  "helloworld.Greeter/SayHello"
```

Using ingress controller:
```sh
grpcurl \
  -H "Cookie: yc_session ${AUTH_COOKIE_VALUE}" \
  "ae5cf6f5.my-cluster.ytsaurus.example.net:9090" \
  "helloworld.Greeter/SayHello"
```

In last example we illustrated what you can also use auth cookie to make requests, auth cookie has name `yc_session` is this case. `9090` is separate gRPC port in your ingress, port value may differ in your case.

## Ingress controller tuning

Here is the example of how to tune [Gwin](https://yandex.cloud/en/docs/application-load-balancer/tools/gwin) ingress controller for task proxy, specifically its gateway and routes.

Add routes for HTTP and gRPC protocols:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: task-proxy-http-https-route
  namespace: {{.Namespace}}
spec:
  hostnames:
    - "{{.TaskProxyPublicFQDN}}"
  parentRefs:
    - name: gateway
      sectionName: yt-task-proxy-http-https-listener
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - kind: Service
          name: task-proxy
          port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: task-proxy-grpc-https-route
  namespace: {{.Namespace}}
spec:
  hostnames:
    - "{{.TaskProxyPublicFQDN}}"
  parentRefs:
    - name: gateway
      sectionName: yt-task-proxy-grpc-https-listener
  rules:
    - backendRefs:
        - kind: Service
          name: task-proxy
          port: 80
```

Add corresponding listeners two gateway's `spec.listeners` section:
```yaml
...
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: gateway
  ...
spec:
  listeners:
    ...
    - name: yt-task-proxy-http-https-listener
      protocol: HTTPS
      port: 443
      hostname: "{{.TaskProxyPublicFQDN}}"
      tls:
        certificateRefs:
          - ...
    - name: yt-task-proxy-grpc-https-listener
      protocol: HTTPS
      port: 9090
      hostname: "{{.TaskProxyPublicFQDN}}"
      tls:
        certificateRefs:
          - ...
```

## TLS support

If you need task proxy to work using TLS, add these values to Helm chart:
```yaml
tls:
  enabled: true
  secretName: yt-domain-cert
```

Here is the example of how to create secret `yt-domain-cert` using [Yandex Cloud](https://yandex.cloud) infrastructure with already existing certificate:
```sh
yc certificate-manager certificate content --id fpqcj5ocpqiav3si9v4g --chain cert.pem --key key.pem
kubectl create secret tls yt-domain-cert -n yt --cert=cert.pem --key=key.pem
```

## Development

Install chart to cluster from local directory using:

```sh
helm install task-proxy \
    -n ${NAMESPACE} \
    -f values.yaml \
    ./chart
```
