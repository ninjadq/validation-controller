package handler

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/validation-controller/api/v1alpha1"
)

// newScheme creates a runtime.Scheme containing the types needed for handler tests.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// newTestValidation returns a Validation CR in the given namespace with reasonable defaults.
func newTestValidation(name, namespace string) *v1alpha1.Validation {
	return &v1alpha1.Validation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "test-uid-123",
		},
		Spec: v1alpha1.ValidationSpec{
			PrUrl: "https://dev.azure.com/org/project/_git/repo/pullrequest/42",
			Template: corev1.Container{
				Name:  "test",
				Image: "busybox:latest",
			},
			MaxRetries: 3,
		},
	}
}

// newHandler creates a ValidationHandler backed by a fake client. The Validation object is
// pre-loaded into the fake client. Returns the handler, fake client, and recorder.
func newHandler(validation *v1alpha1.Validation) (*ValidationHandler, client.Client, *record.FakeRecorder) {
	s := newScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.Validation{}).
		WithObjects(validation).
		Build()
	recorder := record.NewFakeRecorder(10)
	h := &ValidationHandler{
		validation: validation,
		logger:     logr.Discard(),
		client:     fakeClient,
		recorder:   recorder,
	}
	return h, fakeClient, recorder
}

// --- EnsureInitialized ---

func TestEnsureInitialized(t *testing.T) {
	validation := newTestValidation("init-test", "default")
	// Phase is empty by default.
	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureInitialized(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest {
		t.Fatal("expected ContinueProcessing, got CancelRequest")
	}
	if result.RequeueRequest {
		t.Fatal("expected ContinueProcessing, got RequeueRequest")
	}

	// Verify the status was updated via the client.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}

	if updated.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhasePending, updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 3 {
		t.Fatalf("expected 3 conditions, got %d", len(updated.Status.Conditions))
	}

	// Verify all conditions are False.
	for _, c := range updated.Status.Conditions {
		if c.Status != metav1.ConditionFalse {
			t.Errorf("expected condition %s to be False, got %s", c.Type, c.Status)
		}
	}
}

func TestEnsureInitialized_AlreadyInitialized(t *testing.T) {
	validation := newTestValidation("init-already", "default")
	validation.Status.Phase = v1alpha1.ValidationPhasePending

	h, _, _ := newHandler(validation)

	result, err := h.EnsureInitialized(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Phase should be unchanged.
	if validation.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("phase should remain Pending, got %q", validation.Status.Phase)
	}
}

// --- EnsureSpecCurrent ---

func TestEnsureSpecCurrent_FirstRun(t *testing.T) {
	validation := newTestValidation("spec-first", "default")
	validation.Status.Phase = v1alpha1.ValidationPhasePending
	validation.Status.SpecHash = ""

	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.SpecHash == "" {
		t.Error("expected specHash to be set, got empty")
	}
}

func TestEnsureSpecCurrent_NoChange(t *testing.T) {
	validation := newTestValidation("spec-same", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-same-run-0"

	h, _, _ := newHandler(validation)
	hash := h.computeSpecHash()
	validation.Status.SpecHash = hash

	h, _, _ = newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}

func TestEnsureSpecCurrent_SpecChanged_Running(t *testing.T) {
	validation := newTestValidation("spec-changed", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-changed-run-0"
	validation.Status.SpecHash = "old-hash-value"
	validation.Status.RetryCount = 2

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

	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted, but it still exists")
	}
}

func TestEnsureSpecCurrent_SpecChanged_Succeeded(t *testing.T) {
	validation := newTestValidation("spec-term", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded
	validation.Status.PodName = "spec-term-run-0"
	validation.Status.SpecHash = "old-hash-value"
	validation.Status.RetryCount = 1

	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsureSpecCurrent(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.RequeueRequest {
		t.Fatal("expected Requeue after spec change on terminal phase")
	}

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

func TestEnsureSpecCurrent_ContainerImageChange(t *testing.T) {
	validation := newTestValidation("spec-img", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-img-run-0"

	h, _, _ := newHandler(validation)
	oldHash := h.computeSpecHash()
	validation.Status.SpecHash = oldHash

	validation.Spec.Template.Image = "alpine:3.18"

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

func TestEnsureSpecCurrent_MaxRetriesChange_NoReset(t *testing.T) {
	validation := newTestValidation("spec-retries", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "spec-retries-run-0"

	h, _, _ := newHandler(validation)
	hash := h.computeSpecHash()
	validation.Status.SpecHash = hash

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

// --- EnsurePodExists ---

func TestEnsurePodExists(t *testing.T) {
	validation := newTestValidation("pod-test", "default")
	validation.Status.Phase = v1alpha1.ValidationPhasePending
	validation.Status.RetryCount = 0

	h, fakeClient, _ := newHandler(validation)

	result, err := h.EnsurePodExists(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// List pods and verify one was created.
	podList := &corev1.PodList{}
	if err := fakeClient.List(context.Background(), podList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}
	if len(podList.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(podList.Items))
	}

	pod := podList.Items[0]

	// Pod name should be deterministic: "pod-test-run-0".
	expectedPodName := "pod-test-run-0"
	if pod.Name != expectedPodName {
		t.Errorf("expected pod name %q, got %q", expectedPodName, pod.Name)
	}

	// Container named "ci-runner".
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].Name != "ci-runner" {
		t.Errorf("expected container name %q, got %q", "ci-runner", pod.Spec.Containers[0].Name)
	}

	// VALIDATION_PR_URL env var.
	found := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "VALIDATION_PR_URL" {
			found = true
			if env.Value != validation.Spec.PrUrl {
				t.Errorf("expected VALIDATION_PR_URL=%q, got %q", validation.Spec.PrUrl, env.Value)
			}
		}
	}
	if !found {
		t.Error("VALIDATION_PR_URL env var not found on container")
	}

	// RestartPolicy.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicy %q, got %q", corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	}

	// Owner reference.
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(pod.OwnerReferences))
	}
	if pod.OwnerReferences[0].Name != validation.Name {
		t.Errorf("expected owner name %q, got %q", validation.Name, pod.OwnerReferences[0].Name)
	}

	// Status should be updated to Running.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseRunning {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseRunning, updated.Status.Phase)
	}
	if updated.Status.PodName != expectedPodName {
		t.Errorf("expected PodName %q, got %q", expectedPodName, updated.Status.PodName)
	}
}

func TestEnsurePodExists_AlreadyExists(t *testing.T) {
	validation := newTestValidation("pod-exists", "default")
	validation.Status.Phase = v1alpha1.ValidationPhasePending
	validation.Status.RetryCount = 0

	// Pre-create the pod that would be generated (deterministic name).
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-exists-run-0",
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
		WithObjects(validation, existingPod).
		Build()
	recorder := record.NewFakeRecorder(10)
	h := &ValidationHandler{
		validation: validation,
		logger:     logr.Discard(),
		client:     fakeClient,
		recorder:   recorder,
	}

	result, err := h.EnsurePodExists(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Status should still be updated to Running.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseRunning {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseRunning, updated.Status.Phase)
	}
}

func TestEnsurePodExists_NotPending(t *testing.T) {
	validation := newTestValidation("pod-not-pending", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning

	h, _, _ := newHandler(validation)

	result, err := h.EnsurePodExists(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}

// --- CheckPodStatus ---

func TestCheckPodStatus_Succeeded(t *testing.T) {
	validation := newTestValidation("check-success", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "check-success-run-0"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-success-run-0",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
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

	// Verify status updated to Succeeded.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseSucceeded {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseSucceeded, updated.Status.Phase)
	}

	// Verify conditions.
	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue)
	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestPassed, metav1.ConditionTrue)

	// Verify pod is deleted.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted, but it still exists")
	}
}

func TestCheckPodStatus_Failed(t *testing.T) {
	validation := newTestValidation("check-fail", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "check-fail-run-0"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-fail-run-0",
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

	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue)
	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse)

	// Verify pod is deleted.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted, but it still exists")
	}
}

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

	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue)
	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse)

	// Pod should NOT be deleted — retries are exhausted, keep for debugging.
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

	// Parse the annotation value and verify it is roughly 24h from now.
	cleanupTime, err := time.Parse(time.RFC3339, cleanupAfter)
	if err != nil {
		t.Fatalf("failed to parse cleanup-after annotation %q: %v", cleanupAfter, err)
	}
	expectedMin := time.Now().Add(23 * time.Hour)
	expectedMax := time.Now().Add(25 * time.Hour)
	if cleanupTime.Before(expectedMin) || cleanupTime.After(expectedMax) {
		t.Errorf("cleanup-after time %v is not within 23-25 hours from now", cleanupTime)
	}
}

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

	// Verify status updated to Failed.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseFailed {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseFailed, updated.Status.Phase)
	}

	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue)
	assertCondition(t, updated.Status.Conditions, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse)

	// Pod SHOULD be deleted — retries remain.
	deletedPod := &corev1.Pod{}
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, deletedPod)
	if err == nil {
		t.Error("expected pod to be deleted (retries remain), but it still exists")
	}
}

func TestCheckPodStatus_Running(t *testing.T) {
	validation := newTestValidation("check-running", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "check-running-run-0"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "check-running-run-0",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
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
	if !result.RequeueRequest {
		t.Fatal("expected Requeue, got no requeue")
	}
}

func TestCheckPodStatus_PodNotFound(t *testing.T) {
	validation := newTestValidation("check-notfound", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "missing-pod"

	// No pod created in the fake client.
	h, fakeClient, _ := newHandler(validation)

	result, err := h.CheckPodStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Verify status set to Failed.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhaseFailed {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhaseFailed, updated.Status.Phase)
	}
}

func TestCheckPodStatus_EmptyPodName(t *testing.T) {
	validation := newTestValidation("check-empty-name", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	validation.Status.PodName = "" // Empty pod name

	h, _, _ := newHandler(validation)

	_, err := h.CheckPodStatus(context.Background())
	if err == nil {
		t.Fatal("expected error for empty pod name, got nil")
	}
}

// --- HandleRetry ---

func TestHandleRetry_WithRetriesLeft(t *testing.T) {
	validation := newTestValidation("retry-left", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.RetryCount = 1
	validation.Spec.MaxRetries = 3

	h, fakeClient, _ := newHandler(validation)

	result, err := h.HandleRetry(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// HandleRetry should return Requeue so the next reconcile creates the pod immediately.
	if !result.RequeueRequest {
		t.Fatal("expected Requeue, got no requeue")
	}

	// Verify retryCount incremented and phase reset.
	updated := &v1alpha1.Validation{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(validation), updated); err != nil {
		t.Fatalf("failed to get updated validation: %v", err)
	}
	if updated.Status.RetryCount != 2 {
		t.Errorf("expected retryCount=2, got %d", updated.Status.RetryCount)
	}
	if updated.Status.Phase != v1alpha1.ValidationPhasePending {
		t.Errorf("expected phase %q, got %q", v1alpha1.ValidationPhasePending, updated.Status.Phase)
	}
	if updated.Status.PodName != "" {
		t.Errorf("expected PodName to be cleared, got %q", updated.Status.PodName)
	}
}

func TestHandleRetry_RetriesExhausted(t *testing.T) {
	validation := newTestValidation("retry-exhausted", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed
	validation.Status.RetryCount = 3
	validation.Spec.MaxRetries = 3

	h, _, _ := newHandler(validation)

	result, err := h.HandleRetry(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}

	// Phase should remain Failed and retryCount unchanged.
	if validation.Status.Phase != v1alpha1.ValidationPhaseFailed {
		t.Errorf("expected phase to remain Failed, got %q", validation.Status.Phase)
	}
	if validation.Status.RetryCount != 3 {
		t.Errorf("expected retryCount to remain 3, got %d", validation.Status.RetryCount)
	}
}

// --- UpdatePhase ---

func TestUpdatePhase_Succeeded(t *testing.T) {
	validation := newTestValidation("phase-success", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded

	h, _, _ := newHandler(validation)

	result, err := h.UpdatePhase(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CancelRequest {
		t.Fatal("expected StopProcessing (CancelRequest=true)")
	}
}

func TestUpdatePhase_Failed(t *testing.T) {
	validation := newTestValidation("phase-fail", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseFailed

	h, _, _ := newHandler(validation)

	result, err := h.UpdatePhase(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CancelRequest {
		t.Fatal("expected StopProcessing (CancelRequest=true)")
	}
}

func TestUpdatePhase_Running(t *testing.T) {
	validation := newTestValidation("phase-running", "default")
	validation.Status.Phase = v1alpha1.ValidationPhaseRunning

	h, _, _ := newHandler(validation)

	result, err := h.UpdatePhase(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CancelRequest || result.RequeueRequest {
		t.Fatal("expected ContinueProcessing")
	}
}

// --- NewValidationHandler with mock injection ---

func TestNewValidationHandler_MockInjection(t *testing.T) {
	validation := newTestValidation("mock-inject", "default")

	mockHandler := &ValidationHandler{
		validation: validation,
		logger:     logr.Discard(),
	}

	ctx := context.WithValue(context.Background(), ValidationHandlerContextKey{}, ValidationHandlerInterface(mockHandler))
	result := NewValidationHandler(ctx, validation, logr.Discard(), nil, nil)

	if result != mockHandler {
		t.Error("expected mock handler to be returned from context")
	}
}

func TestNewValidationHandler_NoMock(t *testing.T) {
	validation := newTestValidation("no-mock", "default")

	s := newScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	recorder := record.NewFakeRecorder(10)

	result := NewValidationHandler(context.Background(), validation, logr.Discard(), fakeClient, recorder)

	if result == nil {
		t.Fatal("expected non-nil handler")
	}
}

// --- helpers ---

func assertCondition(t *testing.T, conditions []metav1.Condition, condType string, status metav1.ConditionStatus) {
	t.Helper()
	for _, c := range conditions {
		if c.Type == condType {
			if c.Status != status {
				t.Errorf("condition %s: expected status %s, got %s", condType, status, c.Status)
			}
			return
		}
	}
	t.Errorf("condition %s not found", condType)
}
