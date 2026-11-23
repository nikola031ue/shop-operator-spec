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
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/wallet"
)

var _ = Describe("Wallet reconciler (US1, FR-009, FR-010)", func() {
	var ns string
	BeforeEach(func() { ns = newNamespace() })

	It("registers a checksummed address without storing any key", func() {
		w := newWallet(ns, "reg", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
		Expect(k8sClient.Create(ctx, w)).To(Succeed())
		Eventually(func(g Gomega) {
			got := getWallet(g, w)
			g.Expect(readyOf(got.Status.Conditions)).To(Equal("True/Ready"))
			g.Expect(got.Status.Mode).To(Equal(shophubv1alpha1.WalletModeRegistered))
			g.Expect(got.Status.Address).To(Equal("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"))
			g.Expect(got.Status.KeySecretName).To(BeEmpty())
			g.Expect(got.Finalizers).To(ContainElement(FinalizerInUse))
		}, reasonTimeout).Should(Succeed())
		var secrets corev1.SecretList
		Expect(k8sClient.List(ctx, &secrets, client.InNamespace(ns))).To(Succeed())
		Expect(secrets.Items).To(BeEmpty(), "registered wallets hold no key")
	})

	It("reports the all-lowercase form checksummed", func() {
		w := newWallet(ns, "lower", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed")
		Expect(k8sClient.Create(ctx, w)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getWallet(g, w).Status.Address).To(Equal("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"))
		}, reasonTimeout).Should(Succeed())
	})

	It("reports InvalidChecksum for a mixed-case address with a wrong checksum", func() {
		w := newWallet(ns, "badsum", "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
		Expect(k8sClient.Create(ctx, w)).To(Succeed())
		Eventually(func(g Gomega) {
			got := getWallet(g, w)
			g.Expect(readyOf(got.Status.Conditions)).To(Equal("False/" + ReasonInvalidChecksum))
			g.Expect(got.Status.Address).To(BeEmpty())
		}, reasonTimeout).Should(Succeed())
	})

	Context("generated mode", func() {
		var w *shophubv1alpha1.Wallet
		var sec corev1.Secret

		BeforeEach(func() {
			w = newWallet(ns, "gen", "")
			Expect(k8sClient.Create(ctx, w)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(readyOf(getWallet(g, w).Status.Conditions)).To(Equal("True/Ready"))
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gen-wallet-key"}, &sec)).To(Succeed())
			}, reasonTimeout).Should(Succeed())
		})

		It("stores the key only in an owned, labelled Secret and reports the derived address", func() {
			got := getWallet(Default, w)
			priv := string(sec.Data[WalletKeySecretKey])
			addr, err := wallet.AddressFromPrivateKeyHex(priv)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Status.Mode).To(Equal(shophubv1alpha1.WalletModeGenerated))
			Expect(got.Status.Address).To(Equal(addr))
			Expect(got.Status.KeySecretName).To(Equal("gen-wallet-key"))

			owner := sec.OwnerReferences
			Expect(owner).To(HaveLen(1))
			Expect(owner[0].Kind).To(Equal("Wallet"))
			Expect(owner[0].UID).To(Equal(got.UID))
			Expect(*owner[0].Controller).To(BeTrue())
			Expect(sec.Labels).To(HaveKeyWithValue(LabelManagedBy, ManagedByValue))
			Expect(sec.Labels).To(HaveKeyWithValue(LabelOwner, testOwner))

			By("never exposing the private key in status or logs (SC-007)")
			raw, err := json.Marshal(got)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.ToLower(string(raw))).NotTo(ContainSubstring(strings.ToLower(priv)))
			Expect(logBuf.String()).NotTo(ContainSubstring(priv))
		})

		It("never regenerates the key while the Secret exists", func() {
			before := getWallet(Default, w).Status.Address
			// Force reconciles by touching the Wallet.
			for i := range 3 {
				cur := getWallet(Default, w)
				patch := client.MergeFrom(cur.DeepCopy())
				cur.Annotations = map[string]string{"test/touch": fmt.Sprint(i)}
				Expect(k8sClient.Patch(ctx, cur, patch)).To(Succeed())
			}
			Consistently(func(g Gomega) {
				g.Expect(getWallet(g, w).Status.Address).To(Equal(before))
				var now corev1.Secret
				g.Expect(k8sClient.Get(ctx, keyOf(&sec), &now)).To(Succeed())
				g.Expect(now.Data).To(Equal(sec.Data))
			}, "3s").Should(Succeed())
		})

		It("reports KeyLost and keeps the old address when the key Secret is deleted by hand (G4)", func() {
			before := getWallet(Default, w).Status.Address
			Expect(k8sClient.Delete(ctx, &sec)).To(Succeed())
			Eventually(func(g Gomega) {
				got := getWallet(g, w)
				g.Expect(readyOf(got.Status.Conditions)).To(Equal("False/" + ReasonKeyLost))
				g.Expect(got.Status.Address).To(Equal(before))
			}, reasonTimeout).Should(Succeed())
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(&sec), &corev1.Secret{}))
			}, "3s").Should(BeTrue(), "no replacement key may be generated")
		})
	})

	It("tracks usedBy and blocks deletion while a Shop references the Wallet", func() {
		w := newWallet(ns, "used", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
		Expect(k8sClient.Create(ctx, w)).To(Succeed())
		shop := newShop(ns, "s1", "used", shophubv1alpha1.DatabaseStandard)
		Expect(k8sClient.Create(ctx, shop)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getWallet(g, w).Status.UsedBy).To(Equal([]string{"s1"}))
		}, reasonTimeout).Should(Succeed())

		Expect(k8sClient.Delete(ctx, w)).To(Succeed())
		Eventually(func(g Gomega) {
			got := getWallet(g, w)
			g.Expect(got.DeletionTimestamp).NotTo(BeNil())
			g.Expect(readyOf(got.Status.Conditions)).To(Equal("False/" + ReasonInUse))
		}, reasonTimeout).Should(Succeed())
		Consistently(func() error { return k8sClient.Get(ctx, keyOf(w), &shophubv1alpha1.Wallet{}) }, "2s").Should(Succeed())

		Expect(k8sClient.Delete(ctx, shop)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(w), &shophubv1alpha1.Wallet{}))
		}, reasonTimeout).Should(BeTrue(), "the Wallet goes away once no Shop references it")
	})

	It("moves usedBy from the old to the new Wallet when a Shop switches walletRef", func() {
		a := newWallet(ns, "a", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
		b := newWallet(ns, "b", "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359")
		Expect(k8sClient.Create(ctx, a)).To(Succeed())
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		shop := newShop(ns, "s2", "a", shophubv1alpha1.DatabaseStandard)
		Expect(k8sClient.Create(ctx, shop)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getWallet(g, a).Status.UsedBy).To(Equal([]string{"s2"}))
		}, reasonTimeout).Should(Succeed())

		Eventually(func(g Gomega) { // retry: the Shop reconciler updates status concurrently
			g.Expect(k8sClient.Get(ctx, keyOf(shop), shop)).To(Succeed())
			shop.Spec.WalletRef.Name = "b"
			g.Expect(k8sClient.Update(ctx, shop)).To(Succeed())
		}, reasonTimeout).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getWallet(g, a).Status.UsedBy).To(BeEmpty())
			g.Expect(getWallet(g, b).Status.UsedBy).To(Equal([]string{"s2"}))
		}, reasonTimeout).Should(Succeed())
	})
})
