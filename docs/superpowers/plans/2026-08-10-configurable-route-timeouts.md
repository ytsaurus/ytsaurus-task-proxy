# Configurable Route Timeouts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make task-proxy's upstream connection, response, and stream-idle timeouts configurable globally, with route and stream-idle overrides in a task-proxy operation annotation.

**Architecture:** `main` owns validated global defaults and passes a `TaskProxyTimeoutConfig` to the updater and xDS builder. Discovery stores optional operation-level overrides on `Task`; the xDS builder resolves each task's effective timeout values when constructing every equivalent route action.

**Tech Stack:** Go, Envoy xDS v3 protobufs, Helm, testify.

## Global Constraints

- Defaults preserve existing Envoy behavior: 2s connect, 15s route, 5m stream-idle.
- A zero route or stream-idle timeout explicitly disables that limit; connect timeout must be positive.
- New Go configuration fields require comments explaining behavior and zero-value semantics.
- Timeout overrides must influence snapshot versioning.

---

### Task 1: Timeout model and annotation parsing

**Files:**
- Modify: `server/pkg/task.go`
- Modify: `server/pkg/discovery.go`
- Test: `server/pkg/discovery_test.go`

**Interfaces:**
- Produces `TaskProxyTimeoutConfig` and optional timeout fields on `Task`.
- Produces `parseTaskProxyAnnotation(any) ([]taskServiceInfo, *TaskTimeoutOverrides)`.

- [ ] **Step 1: Write failing annotation tests**

Add a valid `task_proxy` annotation with `route_timeout_seconds: 600` and `stream_idle_timeout_seconds: 120`, and assert the returned operation overrides preserve both durations. Add invalid negative and non-integer values and assert parsing rejects the annotation.

- [ ] **Step 2: Run the focused test and verify it fails**

Run: `cd server/pkg && GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./... -run TestParseTaskProxyAnnotation`

Expected: FAIL because no override values are returned or validated.

- [ ] **Step 3: Add the minimal model and parser changes**

Define documented `TaskProxyTimeoutConfig` defaults and optional per-operation `TaskTimeoutOverrides`. Parse the two optional annotation fields after `enabled`, reject malformed values, and copy the resulting overrides to every discovered task.

- [ ] **Step 4: Run the focused test and verify it passes**

Run: `cd server/pkg && GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./... -run TestParseTaskProxyAnnotation`

Expected: PASS.

### Task 2: xDS effective timeouts

**Files:**
- Modify: `server/pkg/xds.go`
- Modify: `server/pkg/updater.go`
- Test: `server/pkg/xds_test.go`
- Test: `server/pkg/updater_test.go`

**Interfaces:**
- Consumes `TaskProxyTimeoutConfig`, `TaskTimeoutOverrides`.
- Produces xDS `RouteAction.Timeout`, `RouteAction.IdleTimeout`, and task upstream `Cluster.ConnectTimeout`.

- [ ] **Step 1: Write failing xDS tests**

Build a snapshot with defaults and a task override. Assert task clusters use the configured connect timeout, all routes for that task share the expected route timeout and idle timeout, and a zero override serializes as an explicit zero duration.

- [ ] **Step 2: Run the focused test and verify it fails**

Run: `cd server/pkg && GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./... -run TestMakeSnapshotTimeouts`

Expected: FAIL because `makeSnapshot` has no timeout configuration parameter and route actions omit both fields.

- [ ] **Step 3: Add xDS resolution and updater plumbing**

Pass the global config from updater to `makeSnapshot`; resolve per-task overrides while building the shared route action. Apply global connect timeout only to discovered task clusters, preserving the authz cluster's internal timeout. Include timeout overrides in task snapshot identity.

- [ ] **Step 4: Run focused xDS and updater tests**

Run: `cd server/pkg && GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./... -run 'TestMakeSnapshotTimeouts|TestTaskUpdater'`

Expected: PASS.

### Task 3: Runtime and Helm configuration

**Files:**
- Modify: `server/main.go`
- Modify: `chart/values.yaml`
- Modify: `chart/templates/deployment.yaml`
- Test: `server/pkg/xds_test.go`

**Interfaces:**
- Consumes Helm `timeouts.connectTimeoutSeconds`, `timeouts.routeTimeoutSeconds`, `timeouts.streamIdleTimeoutSeconds`.
- Produces validated `TaskProxyTimeoutConfig` passed to `CreateTaskUpdater`.

- [ ] **Step 1: Add a failing configuration validation test where feasible**

Exercise the public timeout-validation helper with a zero connect timeout and negative route or stream-idle timeout, expecting errors; verify zero route and stream-idle are accepted.

- [ ] **Step 2: Run the focused validation test and verify it fails**

Run: `cd server/pkg && GOCACHE=/tmp/go-build GOTMPDIR=/tmp go test ./... -run TestValidateTaskProxyTimeoutConfig`

Expected: FAIL because the validation helper does not exist.

- [ ] **Step 3: Implement flags, validation, and chart values**

Add documented timeout fields and validation helper in Go. Add command-line flags and Helm arguments with compatible defaults. Avoid exposing per-operation configuration in Helm because it belongs to the operation annotation.

- [ ] **Step 4: Run complete verification**

Run: `make test && helm template task-proxy ./chart >/tmp/task-proxy-rendered.yaml`

Expected: all Go tests pass and Helm renders successfully.
