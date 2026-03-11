/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// ValidationReconciler reconciles a Validation object.
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

// Reconcile fetches the Validation CR and delegates lifecycle management to the handler.
func (r *ValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx).WithValues("validation", req.NamespacedName)

	validation := &v1alpha1.Validation{}
	if err := r.Get(ctx, req.NamespacedName, validation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling Validation", "phase", validation.Status.Phase)

	h := handler.NewValidationHandler(ctx, validation, logger, r.Client, r.recorder)

	return r.ReconcileHandler(ctx, h)
}

// ReconcileHandler runs all handler operations sequentially. If any operation requests a requeue
// or returns an error, reconciliation stops and requeues. If an operation cancels, reconciliation
// stops without requeueing.
func (r *ValidationReconciler) ReconcileHandler(ctx context.Context, h handler.ValidationHandlerInterface) (ctrl.Result, error) {
	operations := []reconciler.ReconcileOperation{
		h.EnsureInitialized,
		h.EnsurePodExists,
		h.CheckPodStatus,
		h.HandleRetry,
		h.UpdatePhase,
	}

	for _, op := range operations {
		result, err := op(ctx)
		if err != nil || result.RequeueRequest {
			return ctrl.Result{RequeueAfter: result.RequeueDelay}, err
		}
		if result.CancelRequest {
			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{}, nil
}

// validationIndexerFunc indexes pods by their Validation owner for efficient lookups.
func validationIndexerFunc(rawObj client.Object) []string {
	pod, ok := rawObj.(*corev1.Pod)
	if !ok {
		return nil
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return nil
	}

	if owner.APIVersion != v1alpha1.GroupVersion.String() || owner.Kind != "Validation" {
		return nil
	}

	return []string{owner.Name}
}

// SetupWithManager registers the controller with the manager, including field indexers
// and event recording.
func (r *ValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		v1alpha1.ValidationOwnerKey,
		validationIndexerFunc,
	); err != nil {
		return err
	}

	r.recorder = mgr.GetEventRecorderFor("validation-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Validation{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 50}).
		Named("validation").
		Complete(r)
}
