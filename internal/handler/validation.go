package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/validation-controller/api/v1alpha1"
	"github.com/validation-controller/internal/utils/reconciler"
)

// ValidationHandlerContextKey is used to inject a mock handler via context for testing.
type ValidationHandlerContextKey struct{}

//go:generate mockgen -destination=./mocks/mock_validation.go -package=mocks github.com/validation-controller/internal/handler ValidationHandlerInterface

// ValidationHandlerInterface defines the lifecycle operations for a Validation resource.
type ValidationHandlerInterface interface {
	EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error)
	EnsureSpecCurrent(ctx context.Context) (reconciler.OperationResult, error)
	EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error)
	CheckPodStatus(ctx context.Context) (reconciler.OperationResult, error)
	HandleRetry(ctx context.Context) (reconciler.OperationResult, error)
	UpdatePhase(ctx context.Context) (reconciler.OperationResult, error)
}

// ValidationHandler implements ValidationHandlerInterface for managing Validation CR lifecycle.
type ValidationHandler struct {
	validation *v1alpha1.Validation
	logger     logr.Logger
	client     client.Client
	recorder   record.EventRecorder
}

// NewValidationHandler creates a new ValidationHandler. If a mock handler is present in the
// context (via ValidationHandlerContextKey), it returns that instead.
func NewValidationHandler(
	ctx context.Context,
	validation *v1alpha1.Validation,
	logger logr.Logger,
	c client.Client,
	recorder record.EventRecorder,
) ValidationHandlerInterface {
	if h, ok := ctx.Value(ValidationHandlerContextKey{}).(ValidationHandlerInterface); ok {
		return h
	}
	return &ValidationHandler{
		validation: validation,
		logger:     logger,
		client:     c,
		recorder:   recorder,
	}
}

// EnsureInitialized sets the Validation to Pending phase with initial conditions if the phase is empty.
func (h *ValidationHandler) EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseEmpty {
		return reconciler.ContinueProcessing()
	}

	h.logger.Info("Initializing validation resource")

	h.validation.Status.Phase = v1alpha1.ValidationPhasePending

	gen := h.validation.Generation
	h.setCondition(v1alpha1.ValidationConditionPodCreated, metav1.ConditionFalse, "Initializing", "Validation is being initialized", gen)
	h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionFalse, "Initializing", "Validation is being initialized", gen)
	h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "Initializing", "Validation is being initialized", gen)

	h.validation.Status.SpecHash = h.computeSpecHash()

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status during initialization: %w", err))
	}

	h.recorder.Event(h.validation, corev1.EventTypeNormal, "Initialized", "Validation resource initialized")

	return reconciler.ContinueProcessing()
}

// EnsureSpecCurrent detects spec changes and resets the validation if the spec has changed.
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

// EnsurePodExists creates a validation pod if the Validation is in Pending phase.
// Pod names are deterministic based on retry count, making creation idempotent.
func (h *ValidationHandler) EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhasePending {
		return reconciler.ContinueProcessing()
	}

	h.logger.Info("Creating validation pod")

	container := h.validation.Spec.Template.DeepCopy()
	container.Name = "ci-runner"
	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "VALIDATION_PR_URL",
		Value: h.validation.Spec.PrUrl,
	})

	podName := h.generatePodName()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: h.validation.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "validation-controller",
				"validation.devinfra.io/name":  h.validation.Name,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{*container},
		},
	}

	if err := ctrl.SetControllerReference(h.validation, pod, h.client.Scheme()); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("setting owner reference on pod: %w", err))
	}

	if err := h.client.Create(ctx, pod); err != nil {
		if !apierror.IsAlreadyExists(err) {
			return reconciler.RequeueWithError(fmt.Errorf("creating validation pod: %w", err))
		}
		// Pod already exists (e.g., status update failed on previous reconcile).
		// Proceed to update status — the pod is already running.
		h.logger.Info("Pod already exists, proceeding to update status", "pod", podName)
	}

	h.validation.Status.PodName = podName
	h.validation.Status.Phase = v1alpha1.ValidationPhaseRunning

	gen := h.validation.Generation
	h.setCondition(v1alpha1.ValidationConditionPodCreated, metav1.ConditionTrue, "PodCreated", fmt.Sprintf("Pod %s created successfully", podName), gen)

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status after pod creation: %w", err))
	}

	h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "PodCreated", "Created validation pod %s", podName)

	return reconciler.ContinueProcessing()
}

// CheckPodStatus monitors the pod and updates the Validation status based on the pod outcome.
func (h *ValidationHandler) CheckPodStatus(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseRunning {
		return reconciler.ContinueProcessing()
	}

	podName := h.validation.Status.PodName
	if podName == "" {
		return reconciler.RequeueWithError(fmt.Errorf("validation is in Running phase but has no pod name"))
	}

	h.logger.Info("Checking pod status", "pod", podName)

	pod := &corev1.Pod{}
	err := h.client.Get(ctx, client.ObjectKey{
		Namespace: h.validation.Namespace,
		Name:      podName,
	}, pod)

	if apierror.IsNotFound(err) {
		h.logger.Info("Pod not found, marking as failed", "pod", podName)

		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed

		gen := h.validation.Generation
		h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "PodNotFound", fmt.Sprintf("Pod %s was not found", podName), gen)
		h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "PodNotFound", fmt.Sprintf("Pod %s was not found", podName), gen)

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod not found: %w", err))
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeWarning, "PodNotFound", "Validation pod %s not found", podName)

		return reconciler.ContinueProcessing()
	}

	if err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("getting pod %s: %w", podName, err))
	}

	gen := h.validation.Generation

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		h.logger.Info("Pod succeeded", "pod", podName)

		h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "TestSucceeded", "Validation test completed successfully", gen)
		h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionTrue, "TestPassed", "Validation test passed", gen)

		h.validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded

		if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
			return reconciler.RequeueWithError(fmt.Errorf("deleting succeeded pod %s: %w", podName, err))
		}

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod succeeded: %w", err))
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "TestPassed", "Validation test passed in pod %s", podName)

		return reconciler.ContinueProcessing()

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

	default:
		// Pod is still running, requeue to check again
		h.logger.Info("Pod still running, requeueing", "pod", podName, "phase", pod.Status.Phase)
		return reconciler.Requeue()
	}
}

// HandleRetry resets the Validation to Pending if retries remain after a failure.
func (h *ValidationHandler) HandleRetry(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseFailed {
		return reconciler.ContinueProcessing()
	}

	if h.validation.Status.RetryCount >= h.validation.Spec.MaxRetries {
		h.logger.Info("Max retries reached", "retryCount", h.validation.Status.RetryCount, "maxRetries", h.validation.Spec.MaxRetries)
		return reconciler.ContinueProcessing()
	}

	h.validation.Status.RetryCount++
	h.logger.Info("Retrying validation", "retryCount", h.validation.Status.RetryCount, "maxRetries", h.validation.Spec.MaxRetries)

	h.validation.Status.Phase = v1alpha1.ValidationPhasePending
	h.validation.Status.PodName = ""

	gen := h.validation.Generation
	retryMsg := fmt.Sprintf("Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries)
	h.setCondition(v1alpha1.ValidationConditionPodCreated, metav1.ConditionFalse, "Retrying", retryMsg, gen)
	h.setCondition(v1alpha1.ValidationConditionTestCompleted, metav1.ConditionFalse, "Retrying", retryMsg, gen)
	h.setCondition(v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "Retrying", retryMsg, gen)

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status during retry: %w", err))
	}

	h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "Retrying", "Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries)

	return reconciler.Requeue()
}

// UpdatePhase stops processing if the Validation has reached a terminal phase.
func (h *ValidationHandler) UpdatePhase(_ context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase == v1alpha1.ValidationPhaseSucceeded ||
		h.validation.Status.Phase == v1alpha1.ValidationPhaseFailed {
		return reconciler.StopProcessing()
	}
	return reconciler.ContinueProcessing()
}

// setCondition updates or appends a condition using the standard apimeta.SetStatusCondition,
// which correctly handles LastTransitionTime preservation and ObservedGeneration.
func (h *ValidationHandler) setCondition(condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) {
	apimeta.SetStatusCondition(&h.validation.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

// getPodFailureMessage extracts exit code information from the pod's container statuses.
func getPodFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return fmt.Sprintf("Container %s exited with code %d", cs.Name, cs.State.Terminated.ExitCode)
		}
	}
	return "Pod failed with unknown exit code"
}

// generatePodName creates a deterministic pod name based on the retry count.
// This makes pod creation idempotent — if a status update fails after pod creation,
// the next reconcile will attempt to create the same-named pod and get AlreadyExists.
func (h *ValidationHandler) generatePodName() string {
	prefix := h.validation.Name + "-run-"
	suffix := fmt.Sprintf("%d", h.validation.Status.RetryCount)
	maxLen := 63
	if len(prefix)+len(suffix) > maxLen {
		prefix = prefix[:maxLen-len(suffix)]
	}
	return prefix + suffix
}

// computeSpecHash computes a hash of the fields that should trigger a reset when changed.
// Only Template and PrUrl are included — MaxRetries is intentionally excluded.
func (h *ValidationHandler) computeSpecHash() string {
	data := struct {
		Template corev1.Container `json:"template"`
		PrUrl    string           `json:"prUrl"`
	}{
		Template: h.validation.Spec.Template,
		PrUrl:    h.validation.Spec.PrUrl,
	}
	b, _ := json.Marshal(data)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:8])
}
