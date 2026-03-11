# Validation Controller Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a Kubernetes controller that creates pods to run CI tests for Azure DevOps pull requests, triggered by a `Validation` Custom Resource.

**Architecture:** Handler-based reconciliation pattern (from operation-cache-controller). Single CRD (`Validation`) with a reconciler that delegates to a handler interface. The handler creates pods with owner references, monitors their status, manages retries, and cleans up completed pods.

**Tech Stack:** Go 1.24, kubebuilder v4, controller-runtime v0.20.4, Ginkgo v2 + Gomega for tests, go.uber.org/mock for mocks.

---

### Task 1: Scaffold the kubebuilder project

**Files:**
- Create: entire project scaffold at `/home/dq/github/validation-controller/`

**Step 1: Initialize the Go module and kubebuilder project**

```bash
cd /home/dq/github/validation-controller
go mod init github.com/validation-controller
kubebuilder init --domain devinfra.io --repo github.com/validation-controller
```

If `kubebuilder` is not installed, scaffold manually by copying the structure from operation-cache-controller.

**Step 2: Create the Validation API**

```bash
kubebuilder create api --group validation --version v1alpha1 --kind Validation --resource --controller
```

If kubebuilder is not available, create files manually following the patterns from the reference project.

**Step 3: Verify project compiles**

Run: `go build ./...`
Expected: Clean build with no errors.

**Step 4: Initialize git and commit**

```bash
git init
git add -A
git commit -m "feat: scaffold kubebuilder project with Validation CRD"
```

---

### Task 2: Define the Validation CRD types

**Files:**
- Modify: `api/v1alpha1/validation_types.go`

**Step 1: Write the CRD type definitions**

Replace the scaffolded types with:

```go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ValidationOwnerKey = ".validation.metadata.controller"

	// Phase constants
	ValidationPhaseEmpty     = ""
	ValidationPhasePending   = "Pending"
	ValidationPhaseRunning   = "Running"
	ValidationPhaseSucceeded = "Succeeded"
	ValidationPhaseFailed    = "Failed"

	// Condition types
	ValidationConditionPodCreated    = "PodCreated"
	ValidationConditionTestCompleted = "TestCompleted"
	ValidationConditionTestPassed    = "TestPassed"
)

// ValidationSpec defines the desired state of Validation.
type ValidationSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://dev\.azure\.com/.+`
	PrUrl string `json:"prUrl"`
	// +kubebuilder:validation:Required
	Container corev1.Container `json:"container"`
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxRetries int32 `json:"maxRetries,omitempty"`
}

// ValidationStatus defines the observed state of Validation.
type ValidationStatus struct {
	Phase      string             `json:"phase"`
	RetryCount int32              `json:"retryCount"`
	PodName    string             `json:"podName"`
	Conditions []metav1.Condition `json:"conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Retries",type="integer",JSONPath=`.status.retryCount`
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=`.status.podName`

// Validation is the Schema for the validations API.
type Validation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ValidationSpec   `json:"spec,omitempty"`
	Status ValidationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ValidationList contains a list of Validation.
type ValidationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Validation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Validation{}, &ValidationList{})
}
```

**Step 2: Generate CRD manifests and DeepCopy methods**

Run: `make manifests generate`
Expected: Files generated in `config/crd/bases/` and `api/v1alpha1/zz_generated.deepcopy.go` updated.

**Step 3: Verify it compiles**

Run: `go build ./...`
Expected: Clean build.

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: define Validation CRD types with spec, status, and conditions"
```

---

### Task 3: Create the reconciler operation utilities

**Files:**
- Create: `internal/utils/reconciler/operations.go`

**Step 1: Write the operation utilities**

Port the reconciler utilities from `operation-cache-controller/internal/utils/reconciler/operations.go`. This provides `ReconcileOperation`, `OperationResult`, and helper functions (`ContinueProcessing`, `Requeue`, `RequeueWithError`, `StopProcessing`, etc.).

Copy the file verbatim from the reference project (it's generic, no project-specific imports).

**Step 2: Verify it compiles**

Run: `go build ./...`
Expected: Clean build.

**Step 3: Commit**

```bash
git add internal/utils/reconciler/operations.go
git commit -m "feat: add reconciler operation utilities"
```

---

### Task 4: Create the random string utility

**Files:**
- Create: `internal/utils/rand/rand_string.go`

**Step 1: Write the random string generator**

```go
package rand

import (
	"math/rand"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"

// GenerateRandomString generates a random string of length n using lowercase letters and digits.
func GenerateRandomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}
```

**Step 2: Commit**

```bash
git add internal/utils/rand/rand_string.go
git commit -m "feat: add random string utility for pod naming"
```

---

### Task 5: Define the handler interface and implement it

**Files:**
- Create: `internal/handler/validation.go`

**Step 1: Write the handler interface and constructor**

```go
package handler

import (
	"context"
	"fmt"
	"strings"

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

type ValidationHandlerContextKey struct{}

//go:generate mockgen -destination=./mocks/mock_validation.go -package=mocks github.com/validation-controller/internal/handler ValidationHandlerInterface
type ValidationHandlerInterface interface {
	EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error)
	EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error)
	CheckPodStatus(ctx context.Context) (reconciler.OperationResult, error)
	HandleRetry(ctx context.Context) (reconciler.OperationResult, error)
	UpdatePhase(ctx context.Context) (reconciler.OperationResult, error)
}

type ValidationHandler struct {
	validation *v1alpha1.Validation
	logger     logr.Logger
	client     client.Client
	recorder   record.EventRecorder
}

func NewValidationHandler(ctx context.Context, validation *v1alpha1.Validation, logger logr.Logger, c client.Client, recorder record.EventRecorder) ValidationHandlerInterface {
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
```

**Step 2: Implement `EnsureInitialized`**

Initialize the Validation status when first seen (phase is empty). Set phase to `Pending`, initialize conditions to False.

```go
func (h *ValidationHandler) EnsureInitialized(ctx context.Context) (reconciler.OperationResult, error) {
	h.logger.V(1).Info("Operation EnsureInitialized")
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseEmpty {
		return reconciler.ContinueProcessing()
	}

	h.validation.Status.Phase = v1alpha1.ValidationPhasePending
	h.validation.Status.Conditions = []metav1.Condition{
		{
			Type:               v1alpha1.ValidationConditionPodCreated,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "Initializing",
			Message:            "Validation is being initialized",
		},
		{
			Type:               v1alpha1.ValidationConditionTestCompleted,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "Initializing",
			Message:            "Test has not started yet",
		},
		{
			Type:               v1alpha1.ValidationConditionTestPassed,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "Initializing",
			Message:            "Test has not started yet",
		},
	}
	return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))
}
```

**Step 3: Implement `EnsurePodExists`**

If phase is `Pending`, create a pod from the container spec, inject `VALIDATION_PR_URL`, set owner reference. Update status with pod name. Transition to `Running`.

```go
func (h *ValidationHandler) EnsurePodExists(ctx context.Context) (reconciler.OperationResult, error) {
	h.logger.V(1).Info("Operation EnsurePodExists")
	if h.validation.Status.Phase != v1alpha1.ValidationPhasePending {
		return reconciler.ContinueProcessing()
	}

	// Build the pod
	podName := h.generatePodName()
	container := h.validation.Spec.Container.DeepCopy()
	container.Name = "ci-runner"
	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "VALIDATION_PR_URL",
		Value: h.validation.Spec.PrUrl,
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: h.validation.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{*container},
		},
	}

	if err := ctrl.SetControllerReference(h.validation, pod, h.client.Scheme()); err != nil {
		return reconciler.RequeueWithError(fmt.Errorf("failed to set owner reference: %w", err))
	}

	if err := h.client.Create(ctx, pod); err != nil {
		h.recorder.Event(h.validation, "Warning", "FailedCreatePod", err.Error())
		return reconciler.RequeueWithError(fmt.Errorf("failed to create pod %s: %w", podName, err))
	}

	h.validation.Status.PodName = podName
	h.validation.Status.Phase = v1alpha1.ValidationPhaseRunning
	setCondition(h.validation, v1alpha1.ValidationConditionPodCreated, metav1.ConditionTrue, "PodCreated", fmt.Sprintf("Pod %s created", podName))

	h.recorder.Event(h.validation, "Normal", "PodCreated", fmt.Sprintf("Created pod %s", podName))
	return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))
}

func (h *ValidationHandler) generatePodName() string {
	prefix := h.validation.Name + "-run-"
	maxSuffixLen := 63 - len(prefix)
	if maxSuffixLen > 5 {
		maxSuffixLen = 5
	}
	if maxSuffixLen < 1 {
		maxSuffixLen = 1
	}
	return prefix + rand.GenerateRandomString(maxSuffixLen)
}
```

**Step 4: Implement `CheckPodStatus`**

If phase is `Running`, get the pod, check its status. If completed, update conditions and delete the pod.

```go
func (h *ValidationHandler) CheckPodStatus(ctx context.Context) (reconciler.OperationResult, error) {
	h.logger.V(1).Info("Operation CheckPodStatus")
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseRunning {
		return reconciler.ContinueProcessing()
	}

	pod := &corev1.Pod{}
	if err := h.client.Get(ctx, client.ObjectKey{
		Namespace: h.validation.Namespace,
		Name:      h.validation.Status.PodName,
	}, pod); err != nil {
		if apierror.IsNotFound(err) {
			// Pod was deleted externally, mark as failed
			h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed
			setCondition(h.validation, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "PodNotFound", "Pod was not found")
			setCondition(h.validation, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "PodNotFound", "Pod was not found")
			return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))
		}
		return reconciler.RequeueWithError(fmt.Errorf("failed to get pod %s: %w", h.validation.Status.PodName, err))
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		setCondition(h.validation, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "TestCompleted", "Test run completed")
		setCondition(h.validation, v1alpha1.ValidationConditionTestPassed, metav1.ConditionTrue, "TestPassed", "Test passed with exit code 0")
		h.validation.Status.Phase = v1alpha1.ValidationPhaseSucceeded
		h.recorder.Event(h.validation, "Normal", "TestPassed", "CI test passed")
		// Clean up the pod
		if err := h.client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			h.logger.Error(err, "failed to delete succeeded pod", "pod", pod.Name)
		}
		return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))

	case corev1.PodFailed:
		msg := getPodFailureMessage(pod)
		setCondition(h.validation, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionTrue, "TestCompleted", "Test run completed")
		setCondition(h.validation, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "TestFailed", msg)
		h.validation.Status.Phase = v1alpha1.ValidationPhaseFailed
		h.recorder.Event(h.validation, "Warning", "TestFailed", msg)
		// Clean up the pod
		if err := h.client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			h.logger.Error(err, "failed to delete failed pod", "pod", pod.Name)
		}
		return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))

	default:
		// Pod is still running, requeue
		return reconciler.Requeue()
	}
}

func getPodFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("Container %s exited with code %d", cs.Name, cs.State.Terminated.ExitCode)
		}
	}
	return "Pod failed"
}
```

**Step 5: Implement `HandleRetry`**

If phase is `Failed` and retries remain, reset to `Pending` so the next reconcile creates a new pod.

```go
func (h *ValidationHandler) HandleRetry(ctx context.Context) (reconciler.OperationResult, error) {
	h.logger.V(1).Info("Operation HandleRetry")
	if h.validation.Status.Phase != v1alpha1.ValidationPhaseFailed {
		return reconciler.ContinueProcessing()
	}

	if h.validation.Status.RetryCount >= h.validation.Spec.MaxRetries {
		return reconciler.ContinueProcessing()
	}

	h.validation.Status.RetryCount++
	h.validation.Status.Phase = v1alpha1.ValidationPhasePending
	h.validation.Status.PodName = ""
	setCondition(h.validation, v1alpha1.ValidationConditionPodCreated, metav1.ConditionFalse, "Retrying", fmt.Sprintf("Retry %d/%d", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries))
	setCondition(h.validation, v1alpha1.ValidationConditionTestCompleted, metav1.ConditionFalse, "Retrying", "Retrying test")
	setCondition(h.validation, v1alpha1.ValidationConditionTestPassed, metav1.ConditionFalse, "Retrying", "Retrying test")

	h.recorder.Event(h.validation, "Normal", "Retrying", fmt.Sprintf("Retrying validation (%d/%d)", h.validation.Status.RetryCount, h.validation.Spec.MaxRetries))
	return reconciler.RequeueOnErrorOrContinue(h.client.Status().Update(ctx, h.validation))
}
```

**Step 6: Implement `UpdatePhase` (terminal — no-op for now)**

```go
func (h *ValidationHandler) UpdatePhase(ctx context.Context) (reconciler.OperationResult, error) {
	h.logger.V(1).Info("Operation UpdatePhase")
	// Terminal phases — stop processing
	if h.validation.Status.Phase == v1alpha1.ValidationPhaseSucceeded ||
		h.validation.Status.Phase == v1alpha1.ValidationPhaseFailed {
		return reconciler.StopProcessing()
	}
	return reconciler.ContinueProcessing()
}
```

**Step 7: Add the condition helper**

```go
func setCondition(v *v1alpha1.Validation, condType string, status metav1.ConditionStatus, reason, message string) {
	for i, c := range v.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				v.Status.Conditions[i].LastTransitionTime = metav1.Now()
			}
			v.Status.Conditions[i].Status = status
			v.Status.Conditions[i].Reason = reason
			v.Status.Conditions[i].Message = message
			return
		}
	}
	v.Status.Conditions = append(v.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}
```

**Step 8: Verify it compiles**

Run: `go build ./...`
Expected: Clean build.

**Step 9: Commit**

```bash
git add internal/handler/
git commit -m "feat: implement validation handler with pod lifecycle management"
```

---

### Task 6: Implement the Validation controller (reconciler)

**Files:**
- Modify: `internal/controller/validation_controller.go`

**Step 1: Write the reconciler**

```go
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	klog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/validation-controller/api/v1alpha1"
	"github.com/validation-controller/internal/handler"
	"github.com/validation-controller/internal/utils/reconciler"
)

type ValidationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=validation.devinfra.io,resources=validations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=validation.devinfra.io,resources=validations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=validation.devinfra.io,resources=validations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx).WithValues("validation", req.NamespacedName)
	validation := &v1alpha1.Validation{}
	if err := r.Get(ctx, req.NamespacedName, validation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return r.ReconcileHandler(ctx, handler.NewValidationHandler(ctx, validation, logger, r.Client, r.recorder))
}

func (r *ValidationReconciler) ReconcileHandler(ctx context.Context, h handler.ValidationHandlerInterface) (ctrl.Result, error) {
	operations := []reconciler.ReconcileOperation{
		h.EnsureInitialized,
		h.EnsurePodExists,
		h.CheckPodStatus,
		h.HandleRetry,
		h.UpdatePhase,
	}

	for _, operation := range operations {
		operationResult, err := operation(ctx)
		if err != nil || operationResult.RequeueRequest {
			return ctrl.Result{RequeueAfter: operationResult.RequeueDelay}, err
		}
		if operationResult.CancelRequest {
			return ctrl.Result{}, nil
		}
	}
	return ctrl.Result{}, nil
}

func validationIndexerFunc(rawObj client.Object) []string {
	pod := rawObj.(*corev1.Pod)
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return nil
	}
	if owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Kind != "Validation" {
		return nil
	}
	return []string{owner.Name}
}

func (r *ValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, v1alpha1.ValidationOwnerKey, validationIndexerFunc); err != nil {
		return err
	}

	r.recorder = mgr.GetEventRecorderFor("Validation")

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Validation{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 50,
		}).
		Named("validation").
		Complete(r)
}
```

**Step 2: Update `cmd/main.go` to register only the ValidationReconciler**

The scaffolded main.go should already reference it, but make sure it only has the Validation controller setup (remove any scaffolded TODO controllers if needed).

**Step 3: Generate RBAC manifests**

Run: `make manifests`
Expected: RBAC manifests updated in `config/rbac/`.

**Step 4: Verify it compiles**

Run: `go build ./...`
Expected: Clean build.

**Step 5: Commit**

```bash
git add -A
git commit -m "feat: implement validation reconciler with pod lifecycle management"
```

---

### Task 7: Generate mocks and write controller unit tests

**Files:**
- Create: `internal/handler/mocks/mock_validation.go` (generated)
- Modify: `internal/controller/validation_controller_test.go`

**Step 1: Install mockgen and generate mocks**

```bash
go install go.uber.org/mock/mockgen@latest
go generate ./internal/handler/...
```

Expected: `internal/handler/mocks/mock_validation.go` created.

**Step 2: Write the controller test using the same pattern as operation-cache-controller**

```go
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/validation-controller/api/v1alpha1"
	"github.com/validation-controller/internal/handler"
	hmocks "github.com/validation-controller/internal/handler/mocks"
	"github.com/validation-controller/internal/utils/reconciler"
)

var _ = Describe("Validation Controller", func() {
	Context("When setupWithManager is called", func() {
		It("Should setup the controller with the manager", func() {
			mockCtrl := gomock.NewController(GinkgoT())
			defer mockCtrl.Finish()

			k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: scheme.Scheme,
			})
			Expect(err).NotTo(HaveOccurred())

			err = (&ValidationReconciler{
				Client:   k8sManager.GetClient(),
				Scheme:   k8sManager.GetScheme(),
				recorder: k8sManager.GetEventRecorderFor("validation-controller"),
			}).SetupWithManager(k8sManager)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When reconciling a resource", func() {
		const resourceName = "test-validation"
		var (
			mockAdapterCtrl *gomock.Controller
			mockAdapter     *hmocks.MockValidationHandlerInterface
		)
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the Validation resource")
			err := k8sClient.Get(ctx, typeNamespacedName, &v1alpha1.Validation{})
			if err != nil && errors.IsNotFound(err) {
				resource := &v1alpha1.Validation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: v1alpha1.ValidationSpec{
						PrUrl: "https://dev.azure.com/my-org/my-project/_git/my-repo/pullrequest/123",
						Container: corev1.Container{
							Image:   "test-image:latest",
							Command: []string{"echo", "test"},
						},
						MaxRetries: 2,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
			mockAdapterCtrl = gomock.NewController(GinkgoT())
			mockAdapter = hmocks.NewMockValidationHandlerInterface(mockAdapterCtrl)
		})

		AfterEach(func() {
			resource := &v1alpha1.Validation{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			controllerReconciler := &ValidationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
			}
			ctx = context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockAdapter)

			mockAdapter.EXPECT().EnsureInitialized(gomock.Any()).Return(reconciler.OperationResult{}, nil)
			mockAdapter.EXPECT().EnsurePodExists(gomock.Any()).Return(reconciler.OperationResult{}, nil)
			mockAdapter.EXPECT().CheckPodStatus(gomock.Any()).Return(reconciler.OperationResult{}, nil)
			mockAdapter.EXPECT().HandleRetry(gomock.Any()).Return(reconciler.OperationResult{}, nil)
			mockAdapter.EXPECT().UpdatePhase(gomock.Any()).Return(reconciler.OperationResult{}, nil)

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should cancel the reconcile loop when an operation cancels", func() {
			controllerReconciler := &ValidationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
			}
			ctx = context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockAdapter)

			mockAdapter.EXPECT().EnsureInitialized(gomock.Any()).Return(reconciler.OperationResult{
				CancelRequest: true,
			}, nil)

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should requeue when an operation returns an error", func() {
			controllerReconciler := &ValidationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
			}
			ctx = context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockAdapter)

			mockAdapter.EXPECT().EnsureInitialized(gomock.Any()).Return(reconciler.OperationResult{}, errors.NewServiceUnavailable("test error"))

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(errors.IsServiceUnavailable(err)).To(BeTrue())
		})
	})

	Context("validationIndexerFunc tests", func() {
		It("should return nil for Pod without owner", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-no-owner", Namespace: "default"},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(BeNil())
		})

		It("should return nil for Pod with non-Validation owner", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-wrong-owner",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       "some-deploy",
							UID:        "12345",
							Controller: &[]bool{true}[0],
						},
					},
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(BeNil())
		})

		It("should return owner name for Pod with Validation owner", func() {
			ownerName := "test-validation"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-with-validation-owner",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: v1alpha1.GroupVersion.String(),
							Kind:       "Validation",
							Name:       ownerName,
							UID:        "67890",
							Controller: &[]bool{true}[0],
						},
					},
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(Equal([]string{ownerName}))
		})
	})
})
```

**Step 3: Write/update `internal/controller/suite_test.go`**

Follow the same pattern as the reference project, but with the Validation scheme.

**Step 4: Run the tests**

Run: `make test`
Expected: All tests pass.

**Step 5: Commit**

```bash
git add -A
git commit -m "feat: add controller unit tests with mock handler"
```

---

### Task 8: Write handler unit tests

**Files:**
- Create: `internal/handler/validation_test.go`

**Step 1: Write handler tests**

Test each handler operation:
- `EnsureInitialized`: verify it sets phase to Pending and initializes conditions.
- `EnsurePodExists`: verify it creates a pod with the right spec, env var injection, owner reference.
- `CheckPodStatus`: verify succeeded/failed/running pod handling and cleanup.
- `HandleRetry`: verify retry logic increments count and resets phase.
- `UpdatePhase`: verify terminal states stop processing.

Use a fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`) and a fake recorder.

**Step 2: Run the tests**

Run: `go test ./internal/handler/... -v`
Expected: All tests pass.

**Step 3: Commit**

```bash
git add internal/handler/validation_test.go
git commit -m "feat: add handler unit tests for all validation operations"
```

---

### Task 9: Add sample CR and verify the full build

**Files:**
- Create: `config/samples/validation_v1alpha1_validation.yaml`

**Step 1: Write the sample CR**

```yaml
apiVersion: validation.devinfra.io/v1alpha1
kind: Validation
metadata:
  name: validation-sample
  namespace: default
spec:
  prUrl: "https://dev.azure.com/my-org/my-project/_git/my-repo/pullrequest/123"
  container:
    image: "alpine:latest"
    command: ["sh", "-c", "echo 'Running CI tests for PR' && exit 0"]
  maxRetries: 1
```

**Step 2: Run full build and test suite**

```bash
make build
make test
```

Expected: Build succeeds, all tests pass.

**Step 3: Commit**

```bash
git add -A
git commit -m "feat: add sample Validation CR and verify full build"
```

---

### Task 10: Write CLAUDE.md for the new project

**Files:**
- Create: `CLAUDE.md`

**Step 1: Write the project guide**

Document the project following the same structure as the reference project's CLAUDE.md: overview, architecture, commands, key files, testing strategy.

**Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: add CLAUDE.md project guide"
```
