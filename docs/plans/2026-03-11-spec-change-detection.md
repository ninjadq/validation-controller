# Spec Change Detection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Detect changes to `container` and `prUrl` fields in a Validation CR spec, delete the existing pod, and restart the lifecycle with the new spec.

**Architecture:** Add a `specHash` field to ValidationStatus that stores a SHA-256 hash of `container` + `prUrl`. A new `EnsureSpecCurrent` handler step compares the stored hash against the current spec on every reconcile. On mismatch, it deletes any existing pod, resets status to Pending (including retry count), stores the new hash, and requeues.

**Tech Stack:** Go, crypto/sha256, encoding/json, controller-runtime fake client, go.uber.org/mock

---

### Task 1: Add SpecHash to ValidationStatus

**Files:**
- Modify: `api/v1alpha1/validation_types.go:54-59`

**Step 1: Add the SpecHash field to ValidationStatus**

```go
// ValidationStatus defines the observed state of Validation.
type ValidationStatus struct {
	Phase      string             `json:"phase"`
	RetryCount int32              `json:"retryCount"`
	PodName    string             `json:"podName"`
	SpecHash   string             `json:"specHash,omitempty"`
	Conditions []metav1.Condition `json:"conditions"`
}
```

**Step 2: Regenerate deepcopy and CRD manifests**

Run: `make manifests generate`
Expected: SUCCESS, `zz_generated.deepcopy.go` updated, CRD YAML updated with `specHash` field

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 4: Commit**

```
git add api/ config/
git commit -m "feat: add specHash field to ValidationStatus"
```

---

### Task 2: Add EnsureSpecCurrent to the handler interface

**Files:**
- Modify: `internal/handler/validation.go:26-32`

**Step 1: Add the method to the interface**

Update `ValidationHandlerInterface` to include the new method:

```go
type ValidationHandlerInterface interface {
	EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error)
	EnsureSpecCurrent(ctx context.Context) (reconciler.OperationResult, error)
	EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error)
	CheckPodStatus(ctx context.Context) (reconciler.OperationResult, error)
	HandleRetry(ctx context.Context) (reconciler.OperationResult, error)
	UpdatePhase(ctx context.Context) (reconciler.OperationResult, error)
}
```

**Step 2: Verify it compiles (it won't yet — that's fine)**

Run: `go build ./internal/handler/...`
Expected: FAIL — `ValidationHandler` doesn't implement `EnsureSpecCurrent` yet. This is expected.

---

### Task 3: Write failing tests for EnsureSpecCurrent

**Files:**
- Modify: `internal/handler/validation_test.go`

**Step 1: Write the test for first-run (empty hash)**

Add after the `TestEnsureInitialized_AlreadyInitialized` test:

```go
// --- EnsureSpecCurrent ---

func TestEnsureSpecCurrent_FirstRun(t *testing.T) {
	validation := newTestValidation("spec-first", "default")
	validation.Status.Phase = v1alpha1.ValidationPhasePending
	validation.Status.SpecHash = "" // No hash stored yet

	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Verify specHash was stored.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.SpecHash == "" {
		t.Error("expected specHash to be set, got empty")
	}
}
```

**Step 2: Write the test for no change (hash matches)**

```go
func TestEnsureSpecCurrent_NoChange(t *testing.T) {
	validation := newTestValidation("spec-same", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-same-run-0"

	// Pre-compute and set the hash.
	h, _, _ := newHandler(validation)
	hash := h.computeSpecHash()
	validation.Status.SpecHash = hash

	// Recreate handler with the pre-set hash in status.
	h, _, _ = newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}
```

**Step 3: Write the test for spec change while Running (pod exists)**

```go
func TestEnsureSpecCurrent_SpecChanged_Running(t *testing.T) {
	validation := newTestValidation("spec-changed", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-changed-run-0"
	validation.Status.SpecHash = "old-hash-value"
	validation.Status.RetryCount = 2

	// Create the pod that should be deleted.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spec-changed-run-0",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "ci-runner", Image: "busybox:latest"}},
		},
	}

	s := newScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.Validation{}).
		WithObjects(validation, pod).
		Build()
	recorder := record.NewFakeRecorder(10)
	h := &ValidationHandler{
		validation: validation,
		logger:     logr.Discard(),
		client:     fakeClient,
		recorder:   recorder,
	}

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RequeueRequest {
		t.Fatal("expected Requeue after spec change")
	}

	// Verify status was reset.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("expected phase Pending, got %q", updated.Status.Phase)
	}
	if updated.Status.PodName != "" {
		t.Errorf("expected PodName cleared, got %q", updated.Status.PodName)
	}
	if updated.Status.RetryCount != 0 {
		t.Errorf("expected RetryCount reset to 0, got %d", updated.Status.RetryCount)
	}
	if updated.Status.SpecHash == "old-hash-value" {
		t.Error("expected specHash to be updated")
	}
	if updated.Status.SpecHash == "" {
		t.Error("expected specHash to be set, got empty")
	}

	// Verify pod was deleted.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted, but it still exists")
	}
}
```

**Step 4: Write the test for spec change on terminal phase (Succeeded, no pod)**

```go
func TestEnsureSpecCurrent_SpecChanged_Succeeded(t *testing.T) {
	validation := newTestValidation("spec-term", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded
	validation.Status.PodName = "spec-term-run-0"
	validation.Status.SpecHash = "old-hash-value"
	validation.Status.RetryCount = 1

	// No pod exists (already cleaned up on completion).
	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RequeueRequest {
		t.Fatal("expected Requeue after spec change on terminal phase")
	}

	// Verify full reset.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("expected phase Pending, got %q", updated.Status.Phase)
	}
	if updated.Status.RetryCount != 0 {
		t.Errorf("expected RetryCount reset to 0, got %d", updated.Status.RetryCount)
	}
}
```

**Step 5: Write the test for container image change detection**

```go
func TestEnsureSpecCurrent_ContainerImageChange(t *testing.T) {
	validation := newTestValidation("spec-img", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-img-run-0"

	// Compute hash with current image.
	h, _, _ := newHandler(validation)
	oldHash := h.computeSpecHash()
	validation.Status.SpecHash = oldHash

	// Now change the image.
	validation.Spec.Container.Image = "alpine:3.18"

	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RequeueRequest {
		t.Fatal("expected Requeue after container image change")
	}

	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("expected phase Pending, got %q", updated.Status.Phase)
	}
}
```

**Step 6: Write the test that maxRetries change does NOT trigger reset**

```go
func TestEnsureSpecCurrent_MaxRetriesChange_NoReset(t *testing.T) {
	validation := newTestValidation("spec-retries", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-retries-run-0"

	// Compute hash with current spec.
	h, _, _ := newHandler(validation)
	hash := h.computeSpecHash()
	validation.Status.SpecHash = hash

	// Change only maxRetries.
	validation.Spec.MaxRetries = 10

	h, _, _ = newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing — maxRetries change should not trigger reset")
	}
}
```

**Step 7: Run tests to verify they fail**

Run: `go test ./internal/handler/... -v -run TestEnsureSpecCurrent`
Expected: FAIL — `EnsureSpecCurrent` and `computeSpecHash` don't exist yet

---

### Task 4: Implement computeSpecHash and EnsureSpecCurrent

**Files:**
- Modify: `internal/handler/validation.go`

**Step 1: Add imports for crypto/sha256 and encoding/json**

Add `"crypto/sha256"` and `"encoding/json"` to the import block.

**Step 2: Implement computeSpecHash**

Add after `generatePodName()`:

```go
// computeSpecHash returns a deterministic hash of the container and prUrl fields.
// Changes to maxRetries do not affect the hash.
func (h *ValidationHandler) computeSpecHash() string {
	data := struct {
		Container corev1.Container `json:"container"`
		PrUrl     string           `json:"prUrl"`
	}{
		Container: h.validation.Spec.Container,
		PrUrl:     h.validation.Spec.PrUrl,
	}
	b, _ := json.Marshal(data)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:8])
}
```

**Step 3: Implement EnsureSpecCurrent**

Add after `EnsureInitialized()`:

```go
// EnsureSpecCurrent checks if the container or prUrl spec has changed since the last reconcile.
// If changed, it deletes the existing pod, resets status to Pending, and requeues.
func (h *ValidationHandler) EnsureSpecCurrent(ctx context.Context) (reconciler.OperationResult, error) {
	currentHash := h.computeSpecHash()

	// First run or hash not yet stored — just save it.
	if h.validation.Status.SpecHash == "" {
		h.validation.Status.SpecHash = currentHash
		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("storing initial spec hash: %w", err))
		}
		return reconciler.ContinueProcessing()
	}

	// No change — continue.
	if h.validation.Status.SpecHash == currentHash {
		return reconciler.ContinueProcessing()
	}

	// Spec changed — delete existing pod if present.
	h.logger.Info("Spec change detected, resetting validation",
		"oldHash", h.validation.Status.SpecHash,
		"newHash", currentHash,
	)

	if h.validation.Status.PodName != "" {
		pod := &corev1.Pod{}
		err := h.client.Get(ctx, client.ObjectKey{
			Namespace: h.validation.Namespace,
			Name:      h.validation.Status.PodName,
		}, pod)
		if err == nil {
			if delErr := h.client.Delete(ctx, pod); delErr != nil && !apierror.IsNotFound(delErr) {
				return reconciler.RequeueWithError(fmt.Errorf("deleting pod after spec change: %w", delErr))
			}
		} else if !apierror.IsNotFound(err) {
			return reconciler.RequeueWithError(fmt.Errorf("getting pod for spec change cleanup: %w", err))
		}
	}

	// Reset status.
	h.validation.Status.Phase = v1alpha1.ValidationPhasePending
	h.validation.Status.PodName = ""
	h.validation.Status.RetryCount = 0
	h.validation.Status.SpecHash = currentHash

	gen := h.validation.Generation
	h.setCondition(v1alpha1.ValidationConditionPodCreated, metav1.ConditionFalse, "SpecChanged", "Spec changed, restarting validation", gen)
	h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionFalse, "SpecChanged", "Spec changed, restarting validation", gen)
	h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "SpecChanged", "Spec changed, restarting validation", gen)

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status after spec change: %w", err))
	}

	h.recorder.Event(h.validation, corev1.EventTypeNormal, "SpecChanged", "Spec changed, restarting validation")

	return reconciler.Requeue()
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/handler/... -v -run TestEnsureSpecCurrent`
Expected: ALL PASS

**Step 5: Also store initial spec hash in EnsureInitialized**

In `EnsureInitialized`, before the status update, add:

```go
h.validation.Status.SpecHash = h.computeSpecHash()
```

This ensures the hash is set on first initialization, so `EnsureSpecCurrent` sees it as "first run already handled."

**Step 6: Run all handler tests**

Run: `go test ./internal/handler/... -v`
Expected: ALL PASS

**Step 7: Commit**

```
git add internal/handler/
git commit -m "feat: implement spec change detection with hash comparison"
```

---

### Task 5: Update the controller pipeline

**Files:**
- Modify: `internal/controller/validation_controller.go:69-75`

**Step 1: Add EnsureSpecCurrent to the operations list**

```go
operations := []reconciler.ReconcileOperation{
	h.EnsureInitialized,
	h.EnsureSpecCurrent,
	h.EnsurePodExists,
	h.CheckPodStatus,
	h.HandleRetry,
	h.UpdatePhase,
}
```

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: SUCCESS

**Step 3: Commit**

```
git add internal/controller/
git commit -m "feat: add EnsureSpecCurrent to reconciliation pipeline"
```

---

### Task 6: Regenerate mocks and update controller tests

**Files:**
- Regenerate: `internal/handler/mocks/mock_validation.go`
- Modify: `internal/controller/validation_controller_test.go`

**Step 1: Regenerate mocks**

Run: `go generate ./internal/handler/...`
Expected: `mocks/mock_validation.go` updated with `EnsureSpecCurrent` mock method

**Step 2: Update controller test — successful reconcile**

In the test `"should reconcile successfully when all operations succeed"`, add the new mock expectation after `EnsureInitialized`:

```go
mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().CheckPodStatus(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().HandleRetry(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().UpdatePhase(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
```

**Step 3: Update controller test — requeue on EnsureSpecCurrent**

In `ReconcileHandler` describe block, update the existing requeue test to account for the new step:

```go
mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(
	reconcilerutil.OperationResult{RequeueRequest: true, RequeueDelay: reconcilerutil.DefaultRequeueDelay},
	nil,
)
```

**Step 4: Run all tests**

Run: `make test`
Expected: ALL PASS

**Step 5: Commit**

```
git add internal/handler/mocks/ internal/controller/
git commit -m "test: update mocks and controller tests for EnsureSpecCurrent"
```

---

### Task 7: Final verification

**Step 1: Run full build and test suite**

Run: `make build test`
Expected: ALL PASS

**Step 2: Verify CRD manifest has specHash**

Run: `grep specHash config/crd/bases/validation.devinfra.io_validations.yaml`
Expected: `specHash` field present in the CRD spec
