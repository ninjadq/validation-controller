# Failed Pod 24h Retention Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Keep the last failed validation pod around for 24 hours to allow debugging (log inspection, exec, etc.), instead of deleting it immediately.

**Architecture:** When a pod fails and no retries remain (`RetryCount >= MaxRetries`), skip the pod delete and annotate it with a cleanup timestamp 24h in the future. A new `CleanupExpiredPod` handler step runs on every reconcile for Failed validations — if the TTL has expired, it deletes the pod; if not, it requeues after the remaining duration. Pods that fail with retries remaining are still deleted immediately (the retry creates a new pod).

**Tech Stack:** Go, controller-runtime, kubebuilder, fake client for tests

---

### Task 1: Add the cleanup annotation constant

**Files:**
- Modify: `api/v1alpha1/validation_types.go:24-38`

**Step 1: Add the annotation constant**

Add to the `const` block in `api/v1alpha1/validation_types.go`:

```go
// Annotation for deferred pod cleanup
ValidationAnnotationCleanupAfter = "validation.devinfra.io/cleanup-after"
```

Place it after the existing condition type constants (after line 37).

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: Success, no errors.

**Step 3: Commit**

```bash
git add api/v1alpha1/validation_types.go
git commit -m "feat: add cleanup-after annotation constant"
```

---

### Task 2: Modify CheckPodStatus to retain failed pods when retries are exhausted

**Files:**
- Modify: `internal/handler/validation.go:275-295` (the `PodFailed` case)
- Test: `internal/handler/validation_test.go`

**Step 1: Write the failing test — pod retained when retries exhausted**

Add this test to `internal/handler/validation_test.go`, after `TestCheckPodStatus_Failed`:

```go
func TestCheckPodStatus_Failed_RetainsPodWhenRetriesExhausted(t *testing.T) {
	validation := newTestValidation("check-fail-retain", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "check-fail-retain-run-3"
	validation.Status.RetryCount = 3
	validation.Spec.MaxRetries = 3

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-fail-retain-run-3",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "ci-runner",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
						},
					},
				},
			},
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

	result, err := h.CheckPodStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Verify status updated to Failed.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseFailed {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseFailed, updated.Status.Phase)
	}

	// Pod should NOT be deleted — it should still exist.
	retainedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, retainedPod)
	if err != nil {
		t.Fatalf("expected pod to be retained for debugging, but got error: %v", err)
	}

	// Pod should have the cleanup-after annotation.
	cleanupAfter, ok := retainedPod.Annotations[v1alpha1.ValidationAnnotationCleanupAfter]
	if !ok {
		t.Fatal("expected cleanup-after annotation on retained pod")
	}
	if cleanupAfter == "" {
		t.Fatal("cleanup-after annotation should not be empty")
	}
}
```

**Step 2: Write the failing test — pod still deleted when retries remain**

Add this test to verify the existing behavior is preserved when retries remain:

```go
func TestCheckPodStatus_Failed_DeletesPodWhenRetriesRemain(t *testing.T) {
	validation := newTestValidation("check-fail-delete", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "check-fail-delete-run-0"
	validation.Status.RetryCount = 0
	validation.Spec.MaxRetries = 3

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-fail-delete-run-0",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "ci-runner",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
						},
					},
				},
			},
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

	result, err := h.CheckPodStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Pod SHOULD be deleted because retries remain.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted (retries remain), but it still exists")
	}
}
```

**Step 3: Run tests to verify they fail**

Run: `go test ./internal/handler/... -run "TestCheckPodStatus_Failed_(RetainsPod|DeletesPod)" -v`
Expected: FAIL — the new retain test fails because `CheckPodStatus` currently always deletes.

**Step 4: Implement the retention logic in CheckPodStatus**

Modify the `PodFailed` case in `internal/handler/validation.go` (around line 275). Replace the current block:

```go
	case corev1.PodFailed:
		h.logger.Info("Pod failed", "pod", podName)

		failureMessage := getPodFailureMessage(pod)

		h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "TestFailed", "Validation test completed with failure", gen)
		h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "TestFailed", failureMessage, gen)

		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed

		if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
			return reconciler.RequeueWithError(fmt.Errorf("deleting failed pod %s: %w", podName, err))
		}

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod failed: %w", err))
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeWarning, "TestFailed", "Validation test failed in pod %s: %s", podName, failureMessage)

		return reconciler.ContinueProcessing()
```

With:

```go
	case corev1.PodFailed:
		h.logger.Info("Pod failed", "pod", podName)

		failureMessage := getPodFailureMessage(pod)

		h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "TestFailed", "Validation test completed with failure", gen)
		h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "TestFailed", failureMessage, gen)

		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed

		retriesExhausted := h.validation.Status.RetryCount >= h.validation.Spec.MaxRetries
		if retriesExhausted {
			// Keep the pod for debugging — annotate with a 24h cleanup deadline.
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			cleanupTime := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
			pod.Annotations[v1alpha1.ValidationAnnotationCleanupAfter] = cleanupTime
			if err := h.client.Update(ctx, pod); err != nil && !apierror.IsNotFound(err) {
				return reconciler.RequeueWithError(fmt.Errorf("annotating failed pod %s for deferred cleanup: %w", podName, err))
			}
			h.logger.Info("Retaining failed pod for debugging", "pod", podName, "cleanupAfter", cleanupTime)
		} else {
			if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
				return reconciler.RequeueWithError(fmt.Errorf("deleting failed pod %s: %w", podName, err))
			}
		}

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod failed: %w", err))
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeWarning, "TestFailed", "Validation test failed in pod %s: %s", podName, failureMessage)

		return reconciler.ContinueProcessing()
```

Add `"time"` to the imports in `validation.go` if not already present.

**Step 5: Run tests to verify they pass**

Run: `go test ./internal/handler/... -run "TestCheckPodStatus_Failed" -v`
Expected: ALL pass (the original `TestCheckPodStatus_Failed` test needs updating — see Step 6).

**Step 6: Update the existing TestCheckPodStatus_Failed test**

The original `TestCheckPodStatus_Failed` test (line 523) sets `MaxRetries=3` and `RetryCount=0` implicitly (default `newTestValidation` has `MaxRetries=3`, `RetryCount` defaults to 0). Since retries remain, the pod should still be deleted. This test should continue to pass without changes.

However, verify that the test fixture `newTestValidation` has `MaxRetries=3` and the test sets `RetryCount` to 0 (the default) — so retries DO remain, meaning the pod IS deleted. The existing test should pass as-is.

**Step 7: Run all handler tests**

Run: `go test ./internal/handler/... -v`
Expected: ALL pass.

**Step 8: Commit**

```bash
git add internal/handler/validation.go internal/handler/validation_test.go
git commit -m "feat: retain failed pod for 24h debugging when retries exhausted"
```

---

### Task 3: Add CleanupExpiredPod handler method

**Files:**
- Modify: `internal/handler/validation.go` (add new method + interface)
- Test: `internal/handler/validation_test.go`

**Step 1: Write the failing test — expired pod gets cleaned up**

```go
func TestCleanupExpiredPod_DeletesExpiredPod(t *testing.T) {
	validation := newTestValidation("cleanup-expired", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.PodName = "cleanup-expired-run-0"

	// Pod with a cleanup-after annotation in the past.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-expired-run-0",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.ValidationAnnotationCleanupAfter: time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
			},
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

	result, err := h.CleanupExpiredPod(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Pod should be deleted.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted after TTL expiry, but it still exists")
	}
}
```

**Step 2: Write the failing test — non-expired pod is kept and requeues**

```go
func TestCleanupExpiredPod_RequeuesForNonExpiredPod(t *testing.T) {
	validation := newTestValidation("cleanup-pending", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.PodName = "cleanup-pending-run-0"

	// Pod with a cleanup-after annotation 12 hours in the future.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-pending-run-0",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.ValidationAnnotationCleanupAfter: time.Now().Add(12 * time.Hour).Format(time.RFC3339),
			},
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

	result, err := h.CleanupExpiredPod(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RequeueRequest {
		t.Fatal("expected Requeue for non-expired pod")
	}
	// Requeue delay should be roughly 12 hours (within a minute tolerance).
	if result.RequeueDelay < 11*time.Hour || result.RequeueDelay > 13*time.Hour {
		t.Errorf("expected requeue delay ~12h, got %v", result.RequeueDelay)
	}

	// Pod should still exist.
	retainedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, retainedPod)
	if err != nil {
		t.Fatalf("expected pod to still exist, got error: %v", err)
	}
}
```

**Step 3: Write the failing test — no pod name, skip**

```go
func TestCleanupExpiredPod_NoPodName(t *testing.T) {
	validation := newTestValidation("cleanup-nopod", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.PodName = ""

	h, _, _ := newHandler(validation)

	result, err := h.CleanupExpiredPod(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}
```

**Step 4: Write the failing test — pod without annotation, skip**

```go
func TestCleanupExpiredPod_PodWithoutAnnotation(t *testing.T) {
	validation := newTestValidation("cleanup-noanno", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.PodName = "cleanup-noanno-run-0"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-noanno-run-0",
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

	result, err := h.CleanupExpiredPod(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}
```

**Step 5: Write the failing test — not in Failed phase, skip**

```go
func TestCleanupExpiredPod_NotFailed(t *testing.T) {
	validation := newTestValidation("cleanup-running", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning

	h, _, _ := newHandler(validation)

	result, err := h.CleanupExpiredPod(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}
```

**Step 6: Run tests to verify they fail**

Run: `go test ./internal/handler/... -run "TestCleanupExpiredPod" -v`
Expected: FAIL — method does not exist yet.

**Step 7: Add CleanupExpiredPod to the interface**

In `internal/handler/validation.go`, add to `ValidationHandlerInterface` (after `EnsureInitialized`):

```go
CleanupExpiredPod(ctx context.Context) (reconciler.OperationResult, error)
```

**Step 8: Implement CleanupExpiredPod**

Add this method to `ValidationHandler` in `internal/handler/validation.go`, after `EnsureInitialized`:

```go
// CleanupExpiredPod deletes a retained failed pod once its 24h debug window has expired.
func (h *ValidationHandler) CleanupExpiredPod(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseFailed {
		return reconciler.ContinueProcessing()
	}

	podName := h.validation.Status.PodName
	if podName == "" {
		return reconciler.ContinueProcessing()
	}

	pod := &corev1.Pod{}
	err := h.client.Get(ctx, client.ObjectKey{
		Namespace: h.validation.Namespace,
		Name:      podName,
	}, pod)

	if apierror.IsNotFound(err) {
		return reconciler.ContinueProcessing()
	}
	if err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("getting pod %s for cleanup check: %w", podName, err))
	}

	cleanupAfterStr, ok := pod.Annotations[v1alpha1.ValidationAnnotationCleanupAfter]
	if !ok {
		return reconciler.ContinueProcessing()
	}

	cleanupAfter, err := time.Parse(time.RFC3339, cleanupAfterStr)
	if err != nil {
		h.logger.Error(err, "Invalid cleanup-after annotation, deleting pod immediately", "pod", podName)
		if delErr := h.client.Delete(ctx, pod); delErr != nil && !apierror.IsNotFound(delErr) {
			return reconciler.RequeueWithError(fmt.Errorf("deleting pod %s with invalid cleanup annotation: %w", podName, delErr))
		}
		return reconciler.ContinueProcessing()
	}

	remaining := time.Until(cleanupAfter)
	if remaining > 0 {
		h.logger.Info("Pod retained for debugging, cleanup pending", "pod", podName, "cleanupAfter", cleanupAfterStr, "remaining", remaining)
		return reconciler.RequeueAfter(remaining, nil)
	}

	h.logger.Info("Cleanup TTL expired, deleting retained pod", "pod", podName)
	if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
		return reconciler.RequeueWithError(fmt.Errorf("deleting expired pod %s: %w", podName, err))
	}

	h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "PodCleanedUp", "Deleted retained debug pod %s after 24h TTL", podName)

	return reconciler.ContinueProcessing()
}
```

**Step 9: Run tests to verify they pass**

Run: `go test ./internal/handler/... -run "TestCleanupExpiredPod" -v`
Expected: ALL pass.

**Step 10: Run all handler tests**

Run: `go test ./internal/handler/... -v`
Expected: ALL pass.

**Step 11: Commit**

```bash
git add internal/handler/validation.go internal/handler/validation_test.go
git commit -m "feat: add CleanupExpiredPod handler step for deferred pod deletion"
```

---

### Task 4: Wire CleanupExpiredPod into the reconcile pipeline

**Files:**
- Modify: `internal/controller/validation_controller.go:69-76` (operations list)
- Test: `internal/controller/validation_controller_test.go`

**Step 1: Add CleanupExpiredPod to the operations pipeline**

In `internal/controller/validation_controller.go`, update the `operations` slice in `ReconcileHandler` to:

```go
operations := []reconciler.ReconcileOperation{
	h.EnsureInitialized,
	h.CleanupExpiredPod,
	h.EnsureSpecCurrent,
	h.EnsurePodExists,
	h.CheckPodStatus,
	h.HandleRetry,
	h.UpdatePhase,
}
```

`CleanupExpiredPod` goes right after `EnsureInitialized` and before `EnsureSpecCurrent`.

**Step 2: Regenerate mocks**

Run: `go generate ./internal/handler/...`
Expected: `internal/handler/mocks/mock_validation.go` is regenerated with the new `CleanupExpiredPod` method.

**Step 3: Update controller tests — add CleanupExpiredPod mock expectation**

In `internal/controller/validation_controller_test.go`, every test that sets up mock expectations on the full pipeline needs `CleanupExpiredPod` added. Update the "should reconcile successfully" test (around line 88):

```go
mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().CleanupExpiredPod(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().CheckPodStatus(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().HandleRetry(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().UpdatePhase(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
```

And the "should requeue when an operation requests requeue" test (around line 206):

```go
mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().CleanupExpiredPod(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(
	reconcilerutil.OperationResult{RequeueRequest: true, RequeueDelay: reconcilerutil.DefaultRequeueDelay},
	nil,
)
```

**Step 4: Run all tests**

Run: `make test`
Expected: ALL pass.

**Step 5: Commit**

```bash
git add internal/controller/validation_controller.go internal/controller/validation_controller_test.go internal/handler/mocks/mock_validation.go
git commit -m "feat: wire CleanupExpiredPod into reconcile pipeline"
```

---

### Task 5: Update CLAUDE.md documentation

**Files:**
- Modify: `CLAUDE.md`

**Step 1: Update the handler operations list**

In the "Controller Architecture Pattern" section, update the numbered list to include step 2:

```
1. **EnsureInitialized** — Set phase to Pending, initialize conditions
2. **CleanupExpiredPod** — Delete retained debug pods after 24h TTL expires
3. **EnsureSpecCurrent** — Detect spec changes and reset
4. **EnsurePodExists** — Create CI pod with owner reference
5. **CheckPodStatus** — Monitor pod, update conditions; retain last failed pod for 24h if no retries remain
6. **HandleRetry** — Retry on failure if retries remain
7. **UpdatePhase** — Stop processing on terminal phases
```

**Step 2: Add a note to Key Design Decisions**

Add:

```
- **Failed pod retention** — Last failed pod (when retries exhausted) is kept for 24h for debugging, then cleaned up via annotation-based TTL
```

**Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md with failed pod retention behavior"
```
