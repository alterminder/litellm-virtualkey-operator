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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	litellmv1alpha1 "github.com/alterminder/litellm-virtualkey-operator/api/v1alpha1"
)

const modelA = "model-a"

func validSpec() litellmv1alpha1.VirtualKeySpec {
	return litellmv1alpha1.VirtualKeySpec{
		Alias:     "example-key",
		SecretRef: litellmv1alpha1.SecretKeyReference{Name: "example-litellm-key", Key: "API_KEY"},
		Models:    []string{modelA, "model-b"},
		UserID:    "example-user",
		Metadata:  map[string]string{"owner": "example-team"},
	}
}

var _ = Describe("VirtualKey Controller", func() {
	const namespace = "default"
	ctx := context.Background()

	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}

		BeforeEach(func() {
			resource := &litellmv1alpha1.VirtualKey{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       validSpec(),
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &litellmv1alpha1.VirtualKey{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should reject a change to spec.alias", func() {
			resource := &litellmv1alpha1.VirtualKey{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.Alias = "other-alias"
			err := k8sClient.Update(ctx, resource)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
			Expect(err.Error()).To(ContainSubstring("alias is immutable"))
		})

		It("should allow a change to spec.models", func() {
			resource := &litellmv1alpha1.VirtualKey{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.Models = append(resource.Spec.Models, "model-c")
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
		})
	})

	It("should ignore a VirtualKey that does not exist", func() {
		controllerReconciler := &VirtualKeyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "missing", Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should accept the longest alias that reserves the rotation suffix", func() {
		spec := validSpec()
		spec.Alias = strings.Repeat("a", 246)
		resource := &litellmv1alpha1.VirtualKey{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "alias-limit-", Namespace: namespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
	})

	DescribeTable("CRD schema validation rejects",
		func(mutate func(*litellmv1alpha1.VirtualKeySpec)) {
			spec := validSpec()
			mutate(&spec)
			resource := &litellmv1alpha1.VirtualKey{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "invalid-", Namespace: namespace},
				Spec:       spec,
			}
			err := k8sClient.Create(ctx, resource)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
		},
		Entry("an alias too long to reserve the rotation suffix", func(s *litellmv1alpha1.VirtualKeySpec) { s.Alias = strings.Repeat("a", 247) }),
		Entry("an empty alias", func(s *litellmv1alpha1.VirtualKeySpec) { s.Alias = "" }),
		Entry("an empty model list (LiteLLM would allow all models)", func(s *litellmv1alpha1.VirtualKeySpec) { s.Models = nil }),
		Entry("an empty model name", func(s *litellmv1alpha1.VirtualKeySpec) { s.Models = []string{""} }),
		Entry("duplicate model names", func(s *litellmv1alpha1.VirtualKeySpec) { s.Models = []string{modelA, modelA} }),
		Entry("an empty secretRef.name", func(s *litellmv1alpha1.VirtualKeySpec) { s.SecretRef.Name = "" }),
		Entry("an empty secretRef.key", func(s *litellmv1alpha1.VirtualKeySpec) { s.SecretRef.Key = "" }),
	)
})
