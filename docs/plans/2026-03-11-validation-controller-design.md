# Validation Controller Design

## Overview

A Kubernetes controller built with kubebuilder v4 that triggers pods to run CI tests for pull requests. The controller manages a `Validation` Custom Resource that specifies a PR URL (Azure DevOps) and a container to execute the tests.

## CRD: Validation

**API Group:** `validation.devinfra.io/v1alpha1`

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prUrl` | `string` | Yes | Full Azure DevOps PR URL. Contains org, project, repo, and PR ID. |
| `container` | `corev1.Container` | Yes | Kubernetes Container spec for the CI job. |
| `maxRetries` | `int32` | No | Max retry attempts on failure. Default: 0. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current phase: `""` → `Pending` → `Running` → `Succeeded` / `Failed` |
| `retryCount` | `int32` | Number of retries attempted so far. |
| `podName` | `string` | Name of the current/last pod. |
| `conditions` | `[]metav1.Condition` | Standard Kubernetes conditions. |

### Conditions

| Type | Description |
|------|-------------|
| `PodCreated` | Pod has been created for this validation run. |
| `TestCompleted` | Test execution has finished (regardless of result). |
| `TestPassed` | Test passed (exit code 0). `False` when running or failed. |

### Phases

- `""` (empty) — CR just created, not yet processed.
- `Pending` — Controller is creating the pod.
- `Running` — Pod is running the CI test.
- `Succeeded` — Test passed, pod cleaned up.
- `Failed` — Test failed (after all retries exhausted), pod cleaned up.

### Example CR

```yaml
apiVersion: validation.devinfra.io/v1alpha1
kind: Validation
metadata:
  name: pr-validation-123
spec:
  prUrl: "https://dev.azure.com/my-org/my-project/_git/my-repo/pullrequest/123"
  container:
    image: "my-ci-image:latest"
    command: ["./run-tests.sh"]
    env:
      - name: EXTRA_FLAG
        value: "true"
    resources:
      limits:
        cpu: "2"
        memory: "4Gi"
  maxRetries: 2
```

## Architecture

### Pattern

Follows the handler-based reconciliation pattern from `operation-cache-controller`:

```
ValidationReconciler → ValidationHandler (interface) → Handler Implementation
```

### Project Structure

```
validation-controller/
├── api/v1alpha1/
│   └── validation_types.go
├── internal/
│   ├── controller/
│   │   ├── validation_controller.go
│   │   └── validation_controller_test.go
│   ├── handler/
│   │   ├── validation.go
│   │   ├── validation_test.go
│   │   └── mocks/
│   │       └── mock_validation.go
│   └── utils/
│       └── reconciler/
│           └── operations.go
├── cmd/
│   └── main.go
└── config/
```

### Reconciliation Flow

Sequential operations executed in order:

1. **EnsurePodExists** — If no pod is running, create one with owner reference. Transition phase from `""` → `Pending` (pod creating) → `Running` (pod running).
2. **CheckPodStatus** — Watch pod status. On completion, update conditions (`TestCompleted=True`, `TestPassed=True/False`). Delete the completed pod.
3. **HandleRetry** — If failed and `retryCount < maxRetries`, increment `retryCount`, reset conditions, create new pod.
4. **UpdatePhase** — Set final phase to `Succeeded` or `Failed`.

### Pod Creation

- Uses the user-provided `corev1.Container` as the single container.
- Injects `VALIDATION_PR_URL` env var automatically.
- Sets `restartPolicy: Never`.
- Sets owner reference to the Validation CR (enables cascade deletion).
- Pod name format: `{validation-cr-name}-run-{random-suffix}` (max 63 chars).

### Resource Management

- **No finalizers** — owner references handle cascade deletion.
- **Field indexer** on pod owner references for efficient lookups.
- **High concurrency** — ~50 concurrent reconciles.

### RBAC

- `create`, `get`, `list`, `watch`, `delete` on `pods`
- Full CRUD on `validations` and `validations/status`

## Future Considerations

- Support for additional repo providers (GitHub, GitLab) — would require parsing different URL formats.
- Report results back to Azure DevOps PR as a status check.
- Support for multiple containers or init containers.
