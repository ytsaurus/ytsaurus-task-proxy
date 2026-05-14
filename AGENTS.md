# AGENTS.md

## Purpose

This repository contains YTsaurus task proxy: a small Go service that discovers job-local services in YTsaurus and publishes dynamic Envoy xDS config for stable public routing with optional access checks.

## Repository Layout

- `server/main.go` - binary entrypoint. Parses flags, creates the YT client, runs periodic discovery, and serves xDS + `ext_authz` gRPC on port `9090`.
- `server/pkg/discovery.go` - discovers tasks from running operations. Supports SPYT direct submit, SPYT standalone clusters, and generic operations annotated with `task_proxy`.
- `server/pkg/auth.go` - Envoy external authorization backend. Resolves task from host or `x-yt-taskproxy-*` headers and checks YTsaurus operation read permissions.
- `server/pkg/xds.go` - builds Envoy snapshots: listeners, clusters, virtual hosts, header-based routing, optional TLS, and `ext_authz`.
- `server/pkg/updater.go` - applies the latest snapshot to the cache, refreshes auth lookup data, and writes the `services` table to YT.
- `chart/` - Helm chart for deploying the `envoy` + `server` pod.
- `examples/grpc-service/` - sample gRPC service intended to run inside YTsaurus jobs.

## Common Commands

- `make test` - runs unit tests in `server/pkg`.
- `make build` - builds the Linux `amd64` server binary into `server/server`.
- `make image RELEASE_VERSION=<version>` - builds the Docker image.
- `make helm-chart RELEASE_VERSION=<version>` - packages the Helm chart.

If `make test` fails because the Go tool cannot write to its cache in a sandboxed environment, run tests with a writable cache directory, for example:

```sh
cd server/pkg
GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./...
```

## Runtime Model

- Envoy listens on `8080`.
- The Go service serves xDS and `ext_authz` on `9090`.
- Envoy bootstrap config is static in `chart/templates/config.yaml` and points to the local xDS server.
- Dynamic routing is generated from discovered tasks. Each task may be addressed by:
  - a hash-based subdomain
  - an alias-based subdomain when the YT operation has an alias
  - `x-yt-taskproxy-*` routing headers

## Change Guidelines

- Keep changes in `server/pkg/xds.go` and `server/pkg/auth.go` aligned. If you add or rename routing headers or domain formats, update both routing and authorization lookup logic.
- Discovery changes should preserve all currently supported task sources unless the change explicitly removes a scenario.
- `Task.Validate()` constrains alias-based hostnames. If you expand hostname semantics, update validation and tests together.
- When changing chart templates, keep the relationship between:
  - `chart/templates/config.yaml`
  - `chart/templates/deployment.yaml`
  - `server/pkg/const.go`
  consistent for ports, TLS mount paths, and container wiring.

## Testing Expectations

- Update unit tests in `server/pkg/*_test.go` for behavior changes.
- `auth_test.go` covers request-to-task resolution precedence.
- `discovery_test.go` covers parsing of `task_proxy` annotations.
- `xds_test.go` covers the generated Envoy snapshot shape.

## Notes

- The repo may contain local, untracked artifacts during development. Do not delete unrelated files unless the task explicitly asks for cleanup.
