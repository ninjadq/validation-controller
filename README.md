# Validation Controller

A Kubernetes operator that runs CI test pods for Azure DevOps pull requests. Users create a `Validation` custom resource with a PR URL and container spec, and the controller manages the full lifecycle: pod creation, status monitoring, retry on failure, and cleanup.

## How It Works

1. You create a `Validation` resource specifying an Azure DevOps PR URL and a container image
2. The controller creates a pod running your CI tests, injecting `VALIDATION_PR_URL` as an env var
3. It monitors the pod — on success the phase becomes `Succeeded`, on failure it retries (if configured)
4. Cleanup is automatic via owner references — deleting the Validation deletes its pod

Lifecycle phases: `Pending → Running → Succeeded / Failed` (with retry loop back to Pending).

## Prerequisites

- Kubernetes cluster (v1.26+)
- `kubectl` configured to access the cluster
- Docker (for building the controller image)

## Deployment

### Install CRDs

```sh
make install
```

### Build and push the controller image

```sh
make docker-build docker-push IMG=<your-registry>/validation-controller:v0.0.1
```

### Deploy the controller

```sh
make deploy IMG=<your-registry>/validation-controller:v0.0.1
```

### Verify it's running

```sh
kubectl get pods -n validation-controller-system
```

## Usage

Apply a Validation resource to trigger a CI test run:

```yaml
apiVersion: validation.devinfra.io/v1alpha1
kind: Validation
metadata:
  name: my-pr-validation
spec:
  prUrl: "https://dev.azure.com/my-org/my-project/_git/my-repo/pullrequest/123"
  container:
    image: "my-ci-image:latest"
    command: ["sh", "-c", "run-tests.sh"]
  maxRetries: 2
```

Check status:

```sh
kubectl get validations
kubectl describe validation my-pr-validation
```

## Spec Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prUrl` | string | Yes | Azure DevOps PR URL (`https://dev.azure.com/...`) |
| `container` | [Container](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/#Container) | Yes | Container spec for the CI test pod |
| `maxRetries` | int | No | Max retry attempts on failure (default: 0) |

## Uninstall

### Remove the controller

```sh
make undeploy
```

### Remove CRDs

```sh
make uninstall
```