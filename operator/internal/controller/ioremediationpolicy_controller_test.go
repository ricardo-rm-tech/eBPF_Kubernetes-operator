/*
Copyright 2026.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoremediationv1alpha1 "github.com/richi/tfg/operator/api/v1alpha1"
)

var _ = Describe("IORemediationPolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		ioremediationpolicy := &autoremediationv1alpha1.IORemediationPolicy{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind IORemediationPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, ioremediationpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &autoremediationv1alpha1.IORemediationPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: autoremediationv1alpha1.IORemediationPolicySpec{
						TargetPodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "test"},
						},
						PrometheusEndpoint: "http://localhost:9090",
						MetricType:         "IO",
						LatencyThreshold:   "50ms",
						EvaluationWindow:   "5m",
						Action:             "EvictAndTaint",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &autoremediationv1alpha1.IORemediationPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance IORemediationPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should mark the policy Degraded when Prometheus is unreachable", func() {
			By("Reconciling against an unreachable Prometheus endpoint")
			controllerReconciler := &IORemediationPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// El endpoint apunta a localhost:9090 que no existe en envtest.
			// Esperamos que el reconciler devuelva el error de Prometheus
			// y marque la condición Degraded en el status, sin panicar.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred(), "expected reconcile to surface Prometheus error")

			updated := &autoremediationv1alpha1.IORemediationPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())

			var degraded *metav1.Condition
			for i := range updated.Status.Conditions {
				if updated.Status.Conditions[i].Type == "Degraded" {
					degraded = &updated.Status.Conditions[i]
					break
				}
			}
			Expect(degraded).NotTo(BeNil(), "expected Degraded condition to be set")
			Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
			Expect(degraded.Reason).To(Equal("EvaluationFailed"))
		})
	})
})
