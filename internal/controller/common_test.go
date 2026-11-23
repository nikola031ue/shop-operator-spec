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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("common helpers", func() {
	It("setReady sets the Ready condition and observedGeneration, reporting changes", func() {
		var conds []metav1.Condition
		var observed int64

		Expect(setReady(&conds, &observed, 3, false, ReasonProgressing, "pods starting")).To(BeTrue())
		c := meta.FindStatusCondition(conds, ConditionReady)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(ReasonProgressing))
		Expect(c.ObservedGeneration).To(BeEquivalentTo(3))
		Expect(observed).To(BeEquivalentTo(3))

		Expect(setReady(&conds, &observed, 3, false, ReasonProgressing, "pods starting")).To(BeFalse())
		Expect(setReady(&conds, &observed, 4, true, ReasonReady, "ready")).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(conds, ConditionReady)).To(BeTrue())
	})

	It("childLabels copies owner labels only and adds managed-by (FR-019)", func() {
		parent := map[string]string{LabelOwner: "user-42", LabelShop: "acme", "unrelated": "x"}
		got := childLabels(parent)
		Expect(got).To(Equal(map[string]string{
			LabelManagedBy: ManagedByValue, LabelOwner: "user-42", LabelShop: "acme",
		}))
		Expect(parent).To(HaveLen(3), "parent labels must not be modified")
		Expect(childLabels(nil)).To(Equal(map[string]string{LabelManagedBy: ManagedByValue}))
	})
})
