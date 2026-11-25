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
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// Spec 004 FR-026: with --watch-namespace the operator reconciles Shops and Wallets only there,
// and DiscordChannels there and in its own namespace (platform alerts channel). It runs on its
// own API server because the suite's manager watches every namespace.
var _ = Describe("Namespace scoping (spec 004 FR-026)", Ordered, func() {
	const (
		watched  = "shops"
		operator = "shop-operator-system"
		other    = "elsewhere"
	)
	var c client.Client

	BeforeAll(func() {
		env := &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "config", "crd", "bases"),
				filepath.Join("..", "..", "test", "crds"),
			},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: testEnv.BinaryAssetsDirectory,
		}
		scopedCfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)

		c, err = client.New(scopedCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		for _, ns := range []string{watched, operator, other} {
			Expect(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		}

		conf := testConfig
		conf.WatchNamespace = watched
		conf.DiscordTokenSecret = types.NamespacedName{Namespace: operator, Name: testTokenSecret}
		conf.DiscordGuildID = "" // channels report DiscordCredentialsMissing: proof they are reconciled

		mgr, err := ctrl.NewManager(scopedCfg, ctrl.Options{
			Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0", Cache: conf.CacheOptions(),
			Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		mctx, mcancel := context.WithCancel(ctx)
		DeferCleanup(mcancel)
		Expect(SetupIndexes(mctx, mgr)).To(Succeed())
		Expect((&ShopReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Config: conf,
			DBKinds: DatabaseKinds{Postgres: true, Redis: true}, APIReader: mgr.GetAPIReader(),
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&WalletReconciler{
			Config: conf, Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&DiscordChannelReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), Config: conf,
		}).SetupWithManager(mgr)).To(Succeed())
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()
	})

	shopStatus := func(g Gomega, ns string) string {
		var s shophubv1alpha1.Shop
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: testShopName}, &s)).To(Succeed())
		return readyOf(s.Status.Conditions)
	}
	channelStatus := func(g Gomega, ns string) string {
		var d shophubv1alpha1.DiscordChannel
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ch"}, &d)).To(Succeed())
		return readyOf(d.Status.Conditions)
	}

	It("reconciles a Shop, Wallet and DiscordChannel in the watched namespace", func() {
		Expect(c.Create(ctx, newWallet(watched, "w", addrA))).To(Succeed())
		Expect(c.Create(ctx, newDiscordChannel(watched, "ch"))).To(Succeed())
		Expect(c.Create(ctx, newShop(watched, testShopName, "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(shopStatus(g, watched)).To(Equal("False/" + ReasonDiscordChannelNotReady))
			g.Expect(channelStatus(g, watched)).To(Equal("False/" + ReasonDiscordCredentialsMissing))
			g.Expect(readyOf(getWalletFrom(g, c, watched, "w").Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
	})

	It("reconciles the platform DiscordChannel in the operator namespace", func() {
		Expect(c.Create(ctx, newDiscordChannel(operator, "ch"))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(channelStatus(g, operator)).To(Equal("False/" + ReasonDiscordCredentialsMissing))
		}, reasonTimeout).Should(Succeed())
	})

	It("ignores Shops, Wallets and DiscordChannels in any other namespace", func() {
		Expect(c.Create(ctx, newWallet(other, "w", addrA))).To(Succeed())
		Expect(c.Create(ctx, newDiscordChannel(other, "ch"))).To(Succeed())
		Expect(c.Create(ctx, newShop(other, testShopName, "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())
		// A Shop in the operator namespace is not reconciled either: only DiscordChannels are.
		Expect(c.Create(ctx, newShop(operator, testShopName, "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(shopStatus(g, other)).To(BeEmpty())
			g.Expect(shopStatus(g, operator)).To(BeEmpty())
			g.Expect(channelStatus(g, other)).To(BeEmpty())
			g.Expect(getWalletFrom(g, c, other, "w").Status.Conditions).To(BeEmpty())
			g.Expect(getWalletFrom(g, c, other, "w").Finalizers).To(BeEmpty())
		}, "5s", "500ms").Should(Succeed())
		for _, ns := range []string{other, operator} {
			key := client.ObjectKey{Namespace: ns, Name: testShopName}
			Expect(apierrors.IsNotFound(c.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(c.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(c.Get(ctx, key, &networkingv1.NetworkPolicy{}))).To(BeTrue())
		}
	})
})

// getWalletFrom reads a Wallet through client c for assertions inside Eventually.
func getWalletFrom(g Gomega, c client.Client, ns, name string) *shophubv1alpha1.Wallet {
	var w shophubv1alpha1.Wallet
	g.Expect(c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &w)).To(Succeed())
	return &w
}
