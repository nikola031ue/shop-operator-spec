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
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// envtest runs only an API server: no kube-controller-manager, CNPG or Redis operator. Tests
// therefore set Deployment, CNPG Cluster and StatefulSet status by hand to simulate readiness,
// and mark DiscordChannels Ready directly (no DiscordChannel controller in US1).

const (
	addrA = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	addrB = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
)

//nolint:unparam // every Shop fixture references channel "ch"; kept as a parameter for clarity.
func markChannelReady(c client.Client, ns, name string) {
	GinkgoHelper()
	var d shophubv1alpha1.DiscordChannel
	Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
	d.Status.WebhookSecretName = name + "-discord-webhook"
	setReady(&d.Status.Conditions, &d.Status.ObservedGeneration, d.Generation, true, ReasonReady, "test")
	Expect(c.Status().Update(ctx, &d)).To(Succeed())
}

func getShop(g Gomega, ns, name string) *shophubv1alpha1.Shop {
	var s shophubv1alpha1.Shop
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s)).To(Succeed())
	return &s
}

func getDeployment(g Gomega, ns, name string) *appsv1.Deployment {
	var d appsv1.Deployment
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
	return &d
}

func envOf(d *appsv1.Deployment) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		m[e.Name] = e
	}
	return m
}

func markDeploymentAvailable(ns, name string, n int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		d := getDeployment(g, ns, name)
		d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas = n, n, n, n
		d.Status.ObservedGeneration = d.Generation
		g.Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
	}, reasonTimeout).Should(Succeed())
}

func markCNPGReady(ns, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(CNPGClusterGVK)
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u)).To(Succeed())
		g.Expect(unstructured.SetNestedSlice(u.Object, []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "ClusterIsReady", "message": "test",
			"lastTransitionTime": metav1.Now().UTC().Format("2006-01-02T15:04:05Z"),
		}}, "status", "conditions")).To(Succeed())
		g.Expect(k8sClient.Status().Update(ctx, u)).To(Succeed())
	}, reasonTimeout).Should(Succeed())
}

// makeRedisReady creates the StatefulSet the OT operator would create and marks it ready.
func makeRedisReady(ns, name string) {
	GinkgoHelper()
	labels := map[string]string{"app": name}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "redis", Image: "redis:test"}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, sts)).To(Succeed())
	sts.Status.Replicas, sts.Status.ReadyReplicas, sts.Status.ObservedGeneration = 1, 1, sts.Generation
	Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
}

func expectControlledBy(obj metav1.Object, shop *shophubv1alpha1.Shop) {
	GinkgoHelper()
	refs := obj.GetOwnerReferences()
	Expect(refs).To(HaveLen(1), "%s must have exactly one owner", obj.GetName())
	Expect(refs[0].Kind).To(Equal("Shop"))
	Expect(refs[0].UID).To(Equal(shop.UID))
	Expect(refs[0].Controller).NotTo(BeNil())
	Expect(*refs[0].Controller).To(BeTrue())
	Expect(refs[0].BlockOwnerDeletion).NotTo(BeNil())
	Expect(*refs[0].BlockOwnerDeletion).To(BeTrue())
}

var _ = Describe("Shop reconciler (US1)", func() {
	var ns string

	// setupRefs creates a ready Wallet (registered addrA) and a ready DiscordChannel.
	setupRefs := func() {
		Expect(k8sClient.Create(ctx, newWallet(ns, "w", addrA))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "ch"))).To(Succeed())
		markChannelReady(k8sClient, ns, "ch")
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getWallet(g, newWallet(ns, "w", "")).Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
	}

	BeforeEach(func() { ns = newNamespace() })

	It("scenario 1: a standard Shop gets 2 copies, PostgreSQL, the wallet address and becomes Ready", func() {
		setupRefs()
		shop := newShop(ns, testShopName, "w", shophubv1alpha1.DatabaseStandard)
		Expect(k8sClient.Create(ctx, shop)).To(Succeed())

		By("creating the database and app and reporting DatabaseProvisioning")
		Eventually(func(g Gomega) {
			s := getShop(g, ns, testShopName)
			g.Expect(readyOf(s.Status.Conditions)).To(Equal("False/" + ReasonDatabaseProvisioning))
			d := getDeployment(g, ns, testShopName)
			g.Expect(*d.Spec.Replicas).To(BeEquivalentTo(2))
			g.Expect(d.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.27.3"))
			env := envOf(d)
			g.Expect(env["WALLET_ADDRESS"].Value).To(Equal(addrA))
			g.Expect(env["SHOP_NAME"].Value).To(Equal("Shop acme"))
			g.Expect(env["DATABASE_URL"].ValueFrom.SecretKeyRef.Name).To(Equal("acme-db-app"))
		}, reasonTimeout).Should(Succeed())

		shop = getShop(Default, ns, testShopName)
		var svc corev1.Service
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testShopName}, &svc)).To(Succeed())
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(CNPGClusterGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "acme-db"}, cluster)).To(Succeed())

		By("owning every child so deletion removes them (scenario 5, FR-006)")
		expectControlledBy(getDeployment(Default, ns, testShopName), shop)
		expectControlledBy(&svc, shop)
		expectControlledBy(cluster, shop)
		Expect(cluster.GetLabels()).To(HaveKeyWithValue(LabelShop, testShopName))
		Expect(cluster.GetLabels()).To(HaveKeyWithValue(LabelOwner, testOwner))

		By("waiting for pods once the database is ready")
		markCNPGReady(ns, "acme-db")
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, testShopName).Status.Conditions)).To(Equal("False/" + ReasonProgressing))
		}, reasonTimeout).Should(Succeed())

		By("becoming Ready when all copies are available")
		markDeploymentAvailable(ns, testShopName, 2)
		Eventually(func(g Gomega) {
			s := getShop(g, ns, testShopName)
			g.Expect(readyOf(s.Status.Conditions)).To(Equal("True/Ready"))
			g.Expect(s.Status.Replicas).To(BeEquivalentTo(2))
			g.Expect(s.Status.ReadyReplicas).To(BeEquivalentTo(2))
			g.Expect(s.Status.URL).To(Equal("https://acme." + testShopDomain))
			g.Expect(s.Status.WalletAddress).To(Equal(addrA))
			g.Expect(s.Status.WebhookSecretName).To(Equal("ch-discord-webhook"))
			g.Expect(s.Status.ObservedGeneration).To(Equal(s.Generation))
		}, reasonTimeout).Should(Succeed())
	})

	It("spec 004: routes the Shop through an owned Ingress and NetworkPolicy and restores manual edits", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, testShopName, "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())

		key := client.ObjectKey{Namespace: ns, Name: testShopName}
		var ing networkingv1.Ingress
		var np networkingv1.NetworkPolicy
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, &ing)).To(Succeed())
			g.Expect(k8sClient.Get(ctx, key, &np)).To(Succeed())
			// Status is patched at the end of the reconcile, after the children are applied.
			g.Expect(getShop(g, ns, testShopName).Status.URL).To(Equal("https://acme." + testShopDomain))
		}, reasonTimeout).Should(Succeed())
		shop := getShop(Default, ns, testShopName)

		By("owning both, with the configured class, host, namespaces and port")
		expectControlledBy(&ing, shop)
		expectControlledBy(&np, shop)
		Expect(*ing.Spec.IngressClassName).To(Equal("traefik"))
		Expect(ing.Spec.Rules[0].Host).To(Equal("acme." + testShopDomain))
		Expect(ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name).To(Equal(testShopName))
		Expect(ing.Spec.TLS).To(BeEmpty())
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/instance", testShopName))
		Expect(np.Spec.Ingress).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].From).To(HaveLen(2))
		Expect(np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", "traefik"))
		Expect(np.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", "shophub"))
		Expect(envOf(getDeployment(Default, ns, testShopName))["TRUSTED_PROXY_CIDRS"].Value).To(Equal("10.244.0.0/16"))

		By("restoring a hand-edited Ingress host")
		patch := client.MergeFrom(ing.DeepCopy())
		ing.Spec.Rules[0].Host = "evil." + testShopDomain
		Expect(k8sClient.Patch(ctx, &ing, patch)).To(Succeed())
		Eventually(func(g Gomega) {
			var got networkingv1.Ingress
			g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			g.Expect(got.Spec.Rules[0].Host).To(Equal("acme." + testShopDomain))
		}, reasonTimeout).Should(Succeed())

		By("recreating a deleted NetworkPolicy")
		Expect(k8sClient.Delete(ctx, &np)).To(Succeed())
		Eventually(func(g Gomega) {
			var got networkingv1.NetworkPolicy
			g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
			g.Expect(got.UID).NotTo(Equal(np.UID))
		}, reasonTimeout).Should(Succeed())
	})

	It("scenario 2: a light Shop gets Redis instead of PostgreSQL", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, "lite", "w", shophubv1alpha1.DatabaseLight))).To(Succeed())
		Eventually(func(g Gomega) {
			r := &unstructured.Unstructured{}
			r.SetGroupVersionKind(RedisGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "lite-redis"}, r)).To(Succeed())
			g.Expect(envOf(getDeployment(g, ns, "lite"))["REDIS_URL"].Value).To(Equal("redis://lite-redis." + ns + ".svc:6379"))
			g.Expect(readyOf(getShop(g, ns, "lite").Status.Conditions)).To(Equal("False/" + ReasonDatabaseProvisioning))
		}, reasonTimeout).Should(Succeed())
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(CNPGClusterGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "lite-db"}, cluster))).To(BeTrue())

		makeRedisReady(ns, "lite-redis")
		markDeploymentAvailable(ns, "lite", 2)
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, "lite").Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
	})

	It("scenarios 3 and 4: availability and wallet changes update the running shop", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newWallet(ns, "w2", addrB))).To(Succeed())
		Expect(k8sClient.Create(ctx, newShop(ns, "chg", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(*getDeployment(g, ns, "chg").Spec.Replicas).To(BeEquivalentTo(2))
		}, reasonTimeout).Should(Succeed())

		Eventually(func(g Gomega) { // retry: the reconciler updates status concurrently
			s := getShop(g, ns, "chg")
			s.Spec.Availability = shophubv1alpha1.AvailabilityHigh
			s.Spec.WalletRef.Name = "w2"
			g.Expect(k8sClient.Update(ctx, s)).To(Succeed())
		}, reasonTimeout).Should(Succeed())
		Eventually(func(g Gomega) {
			d := getDeployment(g, ns, "chg")
			g.Expect(*d.Spec.Replicas).To(BeEquivalentTo(3))
			g.Expect(envOf(d)["WALLET_ADDRESS"].Value).To(Equal(addrB), "new wallet changes the pod template, so pods roll")
			g.Expect(getShop(g, ns, "chg").Status.WalletAddress).To(Equal(addrB))
		}, reasonTimeout).Should(Succeed())
	})

	It("scenario 6: reports WalletNotReady / DiscordChannelNotReady and recovers by itself", func() {
		Expect(k8sClient.Create(ctx, newShop(ns, "wait", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		Eventually(func(g Gomega) {
			c := getShop(g, ns, "wait").Status.Conditions
			g.Expect(readyOf(c)).To(Equal("False/" + ReasonWalletNotReady))
		}, reasonTimeout).Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "wait"}, &appsv1.Deployment{}))).
			To(BeTrue(), "no app without a wallet address")

		Expect(k8sClient.Create(ctx, newWallet(ns, "w", addrA))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "ch"))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, "wait").Status.Conditions)).To(Equal("False/" + ReasonDiscordChannelNotReady))
		}, reasonTimeout).Should(Succeed())

		markChannelReady(k8sClient, ns, "ch")
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, "wait").Status.Conditions)).To(Equal("False/" + ReasonDatabaseProvisioning))
		}, reasonTimeout).Should(Succeed())
	})

	It("restores a hand-deleted Deployment (FR-018)", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, "heal", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		var uid string
		Eventually(func(g Gomega) { uid = string(getDeployment(g, ns, "heal").UID) }, reasonTimeout).Should(Succeed())

		Expect(k8sClient.Delete(ctx, getDeployment(Default, ns, "heal"))).To(Succeed())
		Eventually(func(g Gomega) {
			d := getDeployment(g, ns, "heal")
			g.Expect(string(d.UID)).NotTo(Equal(uid))
			g.Expect(*d.Spec.Replicas).To(BeEquivalentTo(2))
		}, "30s").Should(Succeed())
	})

	It("reverts a hand-edited replica count (FR-018)", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, "drift", "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		Eventually(func(g Gomega) { getDeployment(g, ns, "drift") }, reasonTimeout).Should(Succeed())
		Eventually(func(g Gomega) {
			d := getDeployment(g, ns, "drift")
			five := int32(5)
			d.Spec.Replicas = &five
			g.Expect(k8sClient.Update(ctx, d)).To(Succeed())
		}, reasonTimeout).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(*getDeployment(g, ns, "drift").Spec.Replicas).To(BeEquivalentTo(2))
		}, "30s").Should(Succeed())
	})
})

var _ = Describe("Shop reconciler without database operators (research R5)", func() {
	It("reports DatabaseOperatorMissing instead of failing", func() {
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: testEnv.BinaryAssetsDirectory,
		}
		bareCfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)
		c, err := client.New(bareCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		kinds, err := DetectDatabaseKinds(bareCfg)
		Expect(err).NotTo(HaveOccurred())
		r := &ShopReconciler{Client: c, Scheme: scheme.Scheme, Config: testConfig, DBKinds: kinds}

		const ns = "default"
		w := newWallet(ns, "w", addrA)
		Expect(c.Create(ctx, w)).To(Succeed())
		w.Status.Address, w.Status.Mode = addrA, shophubv1alpha1.WalletModeRegistered
		setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, true, ReasonReady, "test")
		Expect(c.Status().Update(ctx, w)).To(Succeed())
		Expect(c.Create(ctx, newDiscordChannel(ns, "ch"))).To(Succeed())
		markChannelReady(c, ns, "ch")

		for _, db := range []shophubv1alpha1.DatabaseType{shophubv1alpha1.DatabaseStandard, shophubv1alpha1.DatabaseLight} {
			name := "nodb-" + string(db)
			Expect(c.Create(ctx, newShop(ns, name, "w", db))).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())
			var s shophubv1alpha1.Shop
			Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s)).To(Succeed())
			Expect(readyOf(s.Status.Conditions)).To(Equal("False/" + ReasonDatabaseOperatorMissing))
		}
	})
})
