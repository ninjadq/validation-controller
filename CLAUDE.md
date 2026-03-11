# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## Project Overview

The Validation Controller is a Kubernetes operator built with kubebuilder v4 that triggers pods to run CI tests for Azure DevOps pull requests. Users create a `Validation` Custom Resource specifying a PR URL and container, and the controller manages the full lifecycle: pod creation, status monitoring, retry on failure, and cleanup.

## Architecture

The controller manages one Custom Resource Definition in the `validation.devinfra.io` API group:

**Validation** — Represents a CI test run for a pull request. Contains the PR URL, container spec, and retry configuration.

### Controller Architecture Pattern

```
ValidationReconciler → ValidationHandler (interface) → Handler Implementation
```

The reconciler is in `internal/controller/` and delegates to a handler interface in `internal/handler/`. The handler implements sequential operations:

1. **EnsureInitialized** — Set phase to Pending, initialize conditions
2. **EnsurePodExists** — Create CI pod with owner reference
3. **CheckPodStatus** — Monitor pod, update conditions, clean up on completion
4. **HandleRetry** — Retry on failure if retries remain
5. **UpdatePhase** — Stop processing on terminal phases

### Key Design Decisions

- **No finalizers** — Owner references handle cascade deletion
- **Pod-based** — Uses raw pods (not Jobs) for simplicity
- **VALIDATION_PR_URL env var** — Auto-injected into the CI container
- **Container name forced to "ci-runner"** — Regardless of user input

### Lifecycle Phases

```
"" (empty) → Pending → Running → Succeeded / Failed
                ↑                      |
                └──── (retry) ─────────┘
```

### Conditions

| Type | Description |
|------|-------------|
| PodCreated | Pod has been created |
| TestCompleted | Test execution finished |
| TestPassed | Test passed (exit code 0) |

## Common Development Commands

### Building and Testing
- `make build` — Build the manager binary
- `make test` — Run unit tests with envtest
- `make manifests` — Generate CRD and RBAC manifests
- `make generate` — Generate DeepCopy methods

### Running Specific Tests
- `go test ./internal/controller/... -v` — Controller tests (envtest)
- `go test ./internal/handler/... -v` — Handler tests (fake client)
- `go test -run TestEnsurePodExists -v ./internal/handler/...` — Single test

### Code Generation
- `make manifests generate` — Regenerate CRDs and DeepCopy after type changes
- `go generate ./internal/handler/...` — Regenerate mocks after handler interface changes

## Key Files and Directories

- `api/v1alpha1/validation_types.go` — CRD type definitions
- `internal/controller/validation_controller.go` — Reconciler implementation
- `internal/handler/validation.go` — Handler interface and business logic
- `internal/handler/mocks/` — Generated mocks for testing
- `internal/utils/reconciler/operations.go` — Reconciliation operation helpers
- `internal/utils/rand/rand_string.go` — Random string generator for pod naming
- `config/crd/bases/` — Generated CRD YAML
- `config/rbac/` — Generated RBAC manifests

## Development Environment Requirements

- **Go**: v1.24.0+
- **Docker**: For building container images
- **Tools**: controller-gen, kustomize (auto-installed via Makefile)

## Testing Strategy

- **Controller tests**: Ginkgo v2 + Gomega with envtest (real API server, mocked handler via context injection)
- **Handler tests**: Standard Go tests with fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`)
- **Mock generation**: `go.uber.org/mock` with `//go:generate mockgen` directives

## Development Workflow

1. **CRD Changes**: Update `api/v1alpha1/validation_types.go`
2. **Generate**: Run `make manifests generate`
3. **Handler Logic**: Update `internal/handler/validation.go`
4. **Tests**: Update handler and controller tests
5. **Mocks**: Run `go generate ./internal/handler/...`
