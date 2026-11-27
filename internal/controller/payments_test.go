package controller

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// Spec 005 (operator 0.4.0): the shop's internal token, D-1's hold on an old Wallet, and the
// PaymentsWatching condition that makes a stalled watcher visible.
var _ = Describe("Payments (spec 005)", func() {
	var ns string
	const (
		addrOld = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
		addrNew = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
	)

	BeforeEach(func() { ns = newNamespace() })

	paymentsOf := func(g Gomega, w *shophubv1alpha1.Wallet) string {
		c := meta.FindStatusCondition(getWallet(g, w).Status.Conditions, shophubv1alpha1.ConditionPaymentsWatching)
		if c == nil {
			return ""
		}
		return string(c.Status) + "/" + c.Reason
	}

	// A running shop on the new Wallet, with the old Wallet left over from a wallet change.
	walletChange := func() (*shophubv1alpha1.Wallet, *shophubv1alpha1.Shop) {
		old := newWallet(ns, "acme-w1", addrOld)
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		Expect(k8sClient.Create(ctx, newWallet(ns, "acme-w2", addrNew))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "ch"))).To(Succeed())
		markChannelReady(k8sClient, ns, "ch")
		shop := newShop(ns, testShopName, "acme-w2", shophubv1alpha1.DatabaseStandard)
		Expect(k8sClient.Create(ctx, shop)).To(Succeed())
		markCNPGReady(ns, "acme-db")
		markDeploymentAvailable(ns, testShopName)
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, testShopName).Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
		return old, shop
	}

	It("gives every shop an internal token Secret, by reference, never regenerated", func() {
		_, _ = walletChange()
		var sec corev1.Secret
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testShopName + "-internal"}, &sec)).To(Succeed())
		token := string(sec.Data["token"])
		Expect(token).To(HaveLen(44))
		expectControlledBy(&sec, getShop(Default, ns, testShopName))

		env := envOf(getDeployment(Default, ns, testShopName))
		Expect(env["INTERNAL_TOKEN"].Value).To(BeEmpty())
		Expect(env["INTERNAL_TOKEN"].ValueFrom.SecretKeyRef.Name).To(Equal(testShopName + "-internal"))
		Expect(env["TOKEN_ADDRESS"].Value).To(Equal(testTokenAddress))

		touch(ns, testShopName)
		Consistently(func(g Gomega) {
			var again corev1.Secret
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testShopName + "-internal"}, &again)).To(Succeed())
			g.Expect(string(again.Data["token"])).To(Equal(token))
		}, "2s").Should(Succeed())
		Expect(logBuf.String()).NotTo(ContainSubstring(token))
	})

	It("D-1: keeps an old Wallet while the shop rolls out or has orders waiting on it, then releases it", func() {
		old, _ := walletChange()
		testShops.set(testShopName, 2)
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())

		Eventually(func(g Gomega) {
			c := meta.FindStatusCondition(getWallet(g, old).Status.Conditions, "Ready")
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(ReasonWaitingForOrders))
			g.Expect(c.Message).To(ContainSubstring("2 order(s)"))
		}, reasonTimeout).Should(Succeed())
		Expect(testShops.tokenSeen(testShopName)).To(HaveLen(44), "the shop is asked with its own internal token")

		By("holding while a copy with earlier settings still runs, even with no waiting orders")
		Eventually(func(g Gomega) {
			d := getDeployment(g, ns, testShopName)
			d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas = 3, 3, 3, 2
			g.Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
		}, reasonTimeout).Should(Succeed())
		testShops.set(testShopName, 0)
		Eventually(func(g Gomega) {
			g.Expect(meta.FindStatusCondition(getWallet(g, old).Status.Conditions, "Ready").Message).To(ContainSubstring("rolling out"))
		}, reasonTimeout).Should(Succeed())

		By("releasing once the rollout is done and nothing waits")
		markDeploymentAvailable(ns, testShopName)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(old), &shophubv1alpha1.Wallet{}))
		}, reasonTimeout).Should(BeTrue())
	})

	It("D-1: releases an old Wallet at once when its shop is deleted", func() {
		old, shop := walletChange()
		testShops.set(testShopName, 5)
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getWallet(g, old).Status.Conditions)).To(Equal("False/" + ReasonWaitingForOrders))
		}, reasonTimeout).Should(Succeed())

		Expect(k8sClient.Delete(ctx, shop)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, keyOf(old), &shophubv1alpha1.Wallet{}))
		}, reasonTimeout).Should(BeTrue())
	})

	It("shows PaymentsWatching from the watcher's progress, Stalled once it falls behind", func() {
		w := newWallet(ns, "watched", addrOld)
		Expect(k8sClient.Create(ctx, w)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(paymentsOf(g, w)).To(Equal("False/" + shophubv1alpha1.PaymentsReasonStarting))
		}, reasonTimeout).Should(Succeed())

		setProgress := func(at time.Time, lastError string) {
			Eventually(func(g Gomega) {
				got := getWallet(g, w)
				t := metav1.NewTime(at)
				got.Status.Payments = &shophubv1alpha1.WalletPayments{ScannedThroughBlock: 7000010, ScannedThroughTime: &t, LastError: lastError}
				g.Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
			}, reasonTimeout).Should(Succeed())
		}

		setProgress(time.Now().Add(-90*time.Second), "")
		Eventually(func(g Gomega) {
			g.Expect(paymentsOf(g, w)).To(Equal("True/" + shophubv1alpha1.PaymentsReasonWatching))
		}, reasonTimeout).Should(Succeed())

		setProgress(time.Now().Add(-10*time.Minute), "")
		Eventually(func(g Gomega) {
			g.Expect(paymentsOf(g, w)).To(Equal("False/" + shophubv1alpha1.PaymentsReasonStalled))
		}, reasonTimeout).Should(Succeed())

		setProgress(time.Now().Add(-10*time.Minute), shophubv1alpha1.PaymentsReasonShopUnreachable)
		Eventually(func(g Gomega) {
			g.Expect(paymentsOf(g, w)).To(Equal("False/" + shophubv1alpha1.PaymentsReasonShopUnreachable))
		}, reasonTimeout).Should(Succeed())
	})
})

// The condition as a pure function of configuration, watcher facts and time.
func TestPaymentsWatching(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	on := DefaultPayments("x")
	on.TokenAddress = testTokenAddress
	at := func(ago time.Duration) *shophubv1alpha1.WalletPayments {
		t := metav1.NewTime(now.Add(-ago))
		return &shophubv1alpha1.WalletPayments{ScannedThroughBlock: 9, ScannedThroughTime: &t}
	}
	cases := []struct {
		name   string
		cfg    PaymentsConfig
		p      *shophubv1alpha1.WalletPayments
		ok     bool
		reason string
	}{
		{"payments off", DefaultPayments("x"), at(0), false, shophubv1alpha1.PaymentsReasonTokenNotConfigured},
		{"not read yet", on, nil, false, shophubv1alpha1.PaymentsReasonStarting},
		{"normal lag", on, at(90 * time.Second), true, shophubv1alpha1.PaymentsReasonWatching},
		{"just under the threshold", on, at(5 * time.Minute), true, shophubv1alpha1.PaymentsReasonWatching},
		{"stalled", on, at(5*time.Minute + 2*time.Second), false, shophubv1alpha1.PaymentsReasonStalled},
		{"watcher error wins", on, &shophubv1alpha1.WalletPayments{LastError: shophubv1alpha1.PaymentsReasonProviderError}, false, shophubv1alpha1.PaymentsReasonProviderError},
		{"wrong chain", on, &shophubv1alpha1.WalletPayments{LastError: shophubv1alpha1.PaymentsReasonWrongChain}, false, shophubv1alpha1.PaymentsReasonWrongChain},
		{"read position above the head, even if recent", on, &shophubv1alpha1.WalletPayments{ScannedThroughBlock: 9, ScannedThroughTime: at(0).ScannedThroughTime, LastError: shophubv1alpha1.PaymentsReasonCursorAheadOfChain}, false, shophubv1alpha1.PaymentsReasonCursorAheadOfChain},
	}
	for _, c := range cases {
		ok, reason, _ := PaymentsWatching(c.cfg, c.p, now)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s: got %v/%s, want %v/%s", c.name, ok, reason, c.ok, c.reason)
		}
	}
}

// touch annotates a Shop to make the controller reconcile it again.
func touch(ns, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		s := getShop(g, ns, name)
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations["test.shophub.io/touched"] = time.Now().Format(time.RFC3339Nano)
		g.Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}, reasonTimeout).Should(Succeed())
}
