package handler

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/validation-controller/api/v1alpha1"
	"github.com/validation-controller/internal/utils/rand"
	"github.com/validation-controller/internal/utils/reconciler"
)

// ValidationHandlerContextKey is used to inject a mock handler via context for testing.
type ValidationHandlerContextKey struct{}

//go:generate mockgen -destination=./mocks/mock_validation.go -package=mocks github.com/validation-controller/internal/handler ValidationHandlerInterface

// ValidationHandlerInterface defines the lifecycle operations for a Validation resource.
type ValidationHandlerInterface interface {
	EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error)
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

	now := metav1.Now()
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionPodCreated,
		Status:             metav1.ConditionFalse,
		Reason:             "Initializing",
		Message:            "Validation is being initialized",
		LastTransitionTime: now,
	})
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionTestCompleted,
		Status:             metav1.ConditionFalse,
		Reason:             "Initializing",
		Message:            "Validation is being initialized",
		LastTransitionTime: now,
	})
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionTestPassed,
		Status:             metav1.ConditionFalse,
		Reason:             "Initializing",
		Message:            "Validation is being initialized",
		LastTransitionTime: now,
	})

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status during initialization: %w", err))
	}

	h.recorder.Event(h.validation, corev1.EventTypeNormal, "Initialized", "Validation resource initialized")

	return reconciler.ContinueProcessing()
}

// EnsurePodExists creates a validation pod if the Validation is in Pending phase.
func (h *ValidationHandler) EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase != v1alpha1.ValidationPhasePending {
		return reconciler.ContinueProcessing()
	}

	h.logger.Info("Creating validation pod")

	container := h.validation.Spec.Container.DeepCopy()
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
		return reconciler.RequeueWithError(fmt.Errorf("creating validation pod: %w", err))
	}

	h.validation.Status.PodName = podName
	h.validation.Status.Phase = v1alpha1.ValidationPhaseRunning

	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionPodCreated,
		Status:             metav1.ConditionTrue,
		Reason:             "PodCreated",
		Message:            fmt.Sprintf("Pod %s created successfully", podName),
		LastTransitionTime: metav1.Now(),
	})

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
	h.logger.Info("Checking pod status", "pod", podName)

	pod := &corev1.Pod{}
	err := h.client.Get(ctx, client.ObjectKey{
		Namespace: h.validation.Namespace,
		Name:      podName,
	}, pod)

	if apierror.IsNotFound(err) {
		h.logger.Info("Pod not found, marking as failed", "pod", podName)

		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed

		now := metav1.Now()
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestCompleted,
			Status:             metav1.ConditionTrue,
			Reason:             "PodNotFound",
			Message:            fmt.Sprintf("Pod %s was not found", podName),
			LastTransitionTime: now,
		})
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestPassed,
			Status:             metav1.ConditionFalse,
			Reason:             "PodNotFound",
			Message:            fmt.Sprintf("Pod %s was not found", podName),
			LastTransitionTime: now,
		})

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod not found: %w", err))
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeWarning, "PodNotFound", "Validation pod %s not found", podName)

		return reconciler.ContinueProcessing()
	}

	if err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("getting pod %s: %w", podName, err))
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		h.logger.Info("Pod succeeded", "pod", podName)

		now := metav1.Now()
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestCompleted,
			Status:             metav1.ConditionTrue,
			Reason:             "TestSucceeded",
			Message:            "Validation test completed successfully",
			LastTransitionTime: now,
		})
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestPassed,
			Status:             metav1.ConditionTrue,
			Reason:             "TestPassed",
			Message:            "Validation test passed",
			LastTransitionTime: now,
		})

		h.validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod succeeded: %w", err))
		}

		if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
			h.logger.Error(err, "Failed to delete succeeded pod", "pod", podName)
		}

		h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "TestPassed", "Validation test passed in pod %s", podName)

		return reconciler.ContinueProcessing()

	case corev1.PodFailed:
		h.logger.Info("Pod failed", "pod", podName)

		failureMessage := getPodFailureMessage(pod)

		now := metav1.Now()
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestCompleted,
			Status:             metav1.ConditionTrue,
			Reason:             "TestFailed",
			Message:            "Validation test completed with failure",
			LastTransitionTime: now,
		})
		setCondition(&h.validation.Status, metav1.Condition{
			Type:               v1alpha1.ValidationConditionTestPassed,
			Status:             metav1.ConditionFalse,
			Reason:             "TestFailed",
			Message:            failureMessage,
			LastTransitionTime: now,
		})

		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed

		if err := h.client.Status().Update(ctx, h.validation); err != nil {
			return reconciler.RequeueWithError(fmt.Errorf("updating status after pod failed: %w", err))
		}

		if err := h.client.Delete(ctx, pod); err != nil && !apierror.IsNotFound(err) {
			h.logger.Error(err, "Failed to delete failed pod", "pod", podName)
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

	now := metav1.Now()
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionPodCreated,
		Status:             metav1.ConditionFalse,
		Reason:             "Retrying",
		Message:            fmt.Sprintf("Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries),
		LastTransitionTime: now,
	})
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionTestCompleted,
		Status:             metav1.ConditionFalse,
		Reason:             "Retrying",
		Message:            fmt.Sprintf("Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries),
		LastTransitionTime: now,
	})
	setCondition(&h.validation.Status, metav1.Condition{
		Type:               v1alpha1.ValidationConditionTestPassed,
		Status:             metav1.ConditionFalse,
		Reason:             "Retrying",
		Message:            fmt.Sprintf("Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries),
		LastTransitionTime: now,
	})

	if err := h.client.Status().Update(ctx, h.validation); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("updating status during retry: %w", err))
	}

	h.recorder.Eventf(h.validation, corev1.EventTypeNormal, "Retrying", "Retrying validation, attempt %d of %d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries)

	return reconciler.ContinueProcessing()
}

// UpdatePhase stops processing if the Validation has reached a terminal phase.
func (h *ValidationHandler) UpdatePhase(_ context.Context) (reconciler.OperationResult, error) {
	if h.validation.Status.Phase == v1alpha1.ValidationPhaseSucceeded ||
		h.validation.Status.Phase == v1alpha1.ValidationPhaseFailed {
		return reconciler.StopProcessing()
	}
	return reconciler.ContinueProcessing()
}

// setCondition updates an existing condition or appends a new one. LastTransitionTime is only
// updated when the condition status actually changes.
func setCondition(status *v1alpha1.ValidationStatus, condition metav1.Condition) {
	for i, existing := range status.Conditions {
		if existing.Type == condition.Type {
			if existing.Status != condition.Status {
				status.Conditions[i] = condition
			} else {
				// Status unchanged: preserve the original transition time.
				status.Conditions[i].Reason = condition.Reason
				status.Conditions[i].Message = condition.Message
				status.Conditions[i].LastTransitionTime = existing.LastTransitionTime
			}
			return
		}
	}
	status.Conditions = append(status.Conditions, condition)
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

// generatePodName creates a pod name with a random suffix, truncated to fit the 63-char limit.
func (h *ValidationHandler) generatePodName() string {
	suffix := rand.GenerateRandomString(5)
	prefix := h.validation.Name + "-run-"
	maxLen := 63
	if len(prefix)+len(suffix) > maxLen {
		prefix = prefix[:maxLen-len(suffix)]
	}
	return prefix + suffix
}
