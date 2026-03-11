package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/validation-controller/api/v1alpha1"
	"github.com/validation-controller/internal/handler"
	hmocks "github.com/validation-controller/internal/handler/mocks"
	reconcilerutil "github.com/validation-controller/internal/utils/reconciler"
)

var _ = Describe("ValidationReconciler", func() {

	Describe("SetupWithManager", func() {
		It("should set up the controller with the manager without error", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: scheme.Scheme,
			})
			Expect(err).NotTo(HaveOccurred())

			rec := &ValidationReconciler{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}
			err = rec.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Reconcile", func() {
		var (
			mockCtrl *gomock.Controller
			mockH    *hmocks.MockValidationHandlerInterface
			rec      *ValidationReconciler
			ns       *corev1.Namespace
		)

		BeforeEach(func() {
			mockCtrl = gomock.NewController(GinkgoT())
			mockH = hmocks.NewMockValidationHandlerInterface(mockCtrl)

			rec = &ValidationReconciler{
				Client: k8sClient,
				Scheme: scheme.Scheme,
			}

			// Create a unique namespace for test isolation.
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-ns-",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			mockCtrl.Finish()
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})

		It("should reconcile successfully when all operations succeed", func() {
			validation := &v1alpha1.Validation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-validation",
					Namespace: ns.Name,
				},
				Spec: v1alpha1.ValidationSpec{
					PrUrl: "https://dev.azure.com/test/pr/1",
					Template: corev1.Container{
						Name:  "test",
						Image: "busybox",
					},
				},
			}
			Expect(k8sClient.Create(ctx, validation)).To(Succeed())

			mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().CheckPodStatus(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().HandleRetry(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().UpdatePhase(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)

			ctxWithMock := context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockH)
			result, err := rec.Reconcile(ctxWithMock, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-validation",
					Namespace: ns.Name,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("should stop reconciliation when an operation cancels", func() {
			validation := &v1alpha1.Validation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cancel",
					Namespace: ns.Name,
				},
				Spec: v1alpha1.ValidationSpec{
					PrUrl: "https://dev.azure.com/test/pr/2",
					Template: corev1.Container{
						Name:  "test",
						Image: "busybox",
					},
				},
			}
			Expect(k8sClient.Create(ctx, validation)).To(Succeed())

			mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.StopOperationResult(), nil)

			ctxWithMock := context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockH)
			result, err := rec.Reconcile(ctxWithMock, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-cancel",
					Namespace: ns.Name,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should propagate errors from operations", func() {
			validation := &v1alpha1.Validation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-error",
					Namespace: ns.Name,
				},
				Spec: v1alpha1.ValidationSpec{
					PrUrl: "https://dev.azure.com/test/pr/3",
					Template: corev1.Container{
						Name:  "test",
						Image: "busybox",
					},
				},
			}
			Expect(k8sClient.Create(ctx, validation)).To(Succeed())

			expectedErr := fmt.Errorf("initialization failed")
			mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(
				reconcilerutil.OperationResult{RequeueRequest: true, RequeueDelay: reconcilerutil.DefaultRequeueDelay},
				expectedErr,
			)

			ctxWithMock := context.WithValue(ctx, handler.ValidationHandlerContextKey{}, mockH)
			result, err := rec.Reconcile(ctxWithMock, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-error",
					Namespace: ns.Name,
				},
			})

			Expect(err).To(MatchError("initialization failed"))
			Expect(result.RequeueAfter).To(Equal(reconcilerutil.DefaultRequeueDelay))
		})

		It("should return empty result when Validation is not found", func() {
			result, err := rec.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent",
					Namespace: ns.Name,
				},
			})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Describe("ReconcileHandler", func() {
		var (
			mockCtrl *gomock.Controller
			mockH    *hmocks.MockValidationHandlerInterface
			rec      *ValidationReconciler
		)

		BeforeEach(func() {
			mockCtrl = gomock.NewController(GinkgoT())
			mockH = hmocks.NewMockValidationHandlerInterface(mockCtrl)

			rec = &ValidationReconciler{
				Client: k8sClient,
				Scheme: scheme.Scheme,
			}
		})

		AfterEach(func() {
			mockCtrl.Finish()
		})

		It("should requeue when an operation requests requeue", func() {
			mockH.EXPECT().EnsureInitialized(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().EnsureSpecCurrent(gomock.Any()).Return(reconcilerutil.ContinueOperationResult(), nil)
			mockH.EXPECT().EnsurePodExists(gomock.Any()).Return(
				reconcilerutil.OperationResult{RequeueRequest: true, RequeueDelay: reconcilerutil.DefaultRequeueDelay},
				nil,
			)

			result, err := rec.ReconcileHandler(ctx, mockH)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(reconcilerutil.DefaultRequeueDelay))
		})
	})

	Describe("validationIndexerFunc", func() {
		It("should return nil for a pod without an owner", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-pod",
					Namespace: "default",
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(BeNil())
		})

		It("should return nil for a pod with a wrong owner kind", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-owner-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "ReplicaSet",
							Name:       "some-rs",
							Controller: boolPtr(true),
						},
					},
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(BeNil())
		})

		It("should return the owner name for a pod owned by a Validation", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "validation-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: v1alpha1.GroupVersion.String(),
							Kind:       "Validation",
							Name:       "my-validation",
							Controller: boolPtr(true),
						},
					},
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(Equal([]string{"my-validation"}))
		})

		It("should return nil for a pod with a non-controller Validation owner ref", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-controller-pod",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: v1alpha1.GroupVersion.String(),
							Kind:       "Validation",
							Name:       "my-validation",
							Controller: boolPtr(false),
						},
					},
				},
			}
			result := validationIndexerFunc(pod)
			Expect(result).To(BeNil())
		})

		It("should return nil for a non-pod object", func() {
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "some-service",
					Namespace: "default",
				},
			}
			result := validationIndexerFunc(svc)
			Expect(result).To(BeNil())
		})
	})
})

func boolPtr(b bool) *bool {
	return &b
}
