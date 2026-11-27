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
	"time"

	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

var _ = Describe("Config.Validate (FR-007, spec 004)", func() {
	valid := Config{
		ShopImage: "nginx", ShopImageTag: testImageTag,
		DiscordTokenSecret: types.NamespacedName{Namespace: "shop-operator-system", Name: testTokenSecret},
		WatchNamespace:     "shops", ShopDomain: "shops.localhost", IngressClass: testIngressNS,
		IngressNamespace: testIngressNS, ShopHubNamespace: testShopHub, ShopHubPodSelector: testShopHubPods, TrustedProxyHost: testProxyHost,
		OperatorPodSelector: testOperatorPods, Payments: DefaultPayments("shop-operator-system"),
	}

	It("accepts a pinned image", func() {
		Expect(valid.Validate()).To(Succeed())
	})

	DescribeTable("rejects unpinned or incomplete settings",
		func(mutate func(*Config), fragment string) {
			c := valid
			mutate(&c)
			err := c.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(fragment))
		},
		Entry("latest tag", func(c *Config) { c.ShopImageTag = "latest" }, "pinned version"),
		Entry("empty tag", func(c *Config) { c.ShopImageTag = "" }, "pinned version"),
		Entry("empty image", func(c *Config) { c.ShopImage = "" }, "--shop-image"),
		Entry("no token secret", func(c *Config) { c.DiscordTokenSecret.Name = "" }, "--discord-token-secret"),
		Entry("no watch namespace", func(c *Config) { c.WatchNamespace = "" }, "--watch-namespace"),
		Entry("bad watch namespace", func(c *Config) { c.WatchNamespace = "Shops" }, "--watch-namespace"),
		Entry("no shop domain", func(c *Config) { c.ShopDomain = "" }, "--shop-domain"),
		Entry("no ingress class", func(c *Config) { c.IngressClass = "" }, "--ingress-class"),
		Entry("no ingress namespace", func(c *Config) { c.IngressNamespace = "" }, "--ingress-namespace"),
		Entry("no shophub namespace", func(c *Config) { c.ShopHubNamespace = "" }, "--shophub-namespace"),
		Entry("no shophub pod selector", func(c *Config) { c.ShopHubPodSelector = "" }, "--shophub-pod-selector"),
		Entry("bad shophub pod selector", func(c *Config) { c.ShopHubPodSelector = "app in (a)" }, "--shophub-pod-selector"),
		Entry("CIDR as proxy host", func(c *Config) { c.TrustedProxyHost = "10.244.0.0/16" }, "--trusted-proxy-host"),
		Entry("no operator pod selector", func(c *Config) { c.OperatorPodSelector = "" }, "--operator-pod-selector"),
		Entry("bad token address", func(c *Config) { c.Payments.TokenAddress = "0x123" }, "--payments-token-address"),
		Entry("token checksum typo", func(c *Config) {
			c.Payments.TokenAddress = "0xdF8d5dEb736EBC79f7e99C4c90975d4b8f8d80e2"
		}, "--payments-token-address"),
		Entry("no confirmations", func(c *Config) { c.Payments.Confirmations = 0 }, "--payments-confirmations"),
		Entry("poll too fast", func(c *Config) { c.Payments.PollInterval = 100 * time.Millisecond }, "--payments-poll-interval"),
		Entry("no log range", func(c *Config) { c.Payments.LogsBlockRange = 0 }, "--payments-logs-block-range"),
		Entry("no payment window", func(c *Config) { c.Payments.OrderPaymentMinutes = 0 }, "--payments-order-minutes"),
		Entry("http explorer", func(c *Config) { c.Payments.ExplorerTxURL = "http://x/tx/" }, "--payments-explorer-tx-url"),
		Entry("no RPC secret", func(c *Config) { c.Payments.RPCSecret.Name = "" }, "--payments-rpc-secret"),
		Entry("stall too short", func(c *Config) { c.Payments.StallAfter = time.Second }, "--payments-stall-after"),
	)

	It("spec 005: accepts a token address in any valid form and stores it checksummed", func() {
		c := valid
		c.Payments.TokenAddress = "0xdf8d5deb736ebc79f7e99c4c90975d4b8f8d80e2"
		Expect(c.Validate()).To(Succeed())
		Expect(c.Payments.TokenAddress).To(Equal("0xdF8d5dEb736EBC79f7e99C4c90975d4b8f8d80E2"))
		Expect(c.Payments.Enabled()).To(BeTrue())
		Expect(valid.Payments.Enabled()).To(BeFalse())
	})

	It("accepts an empty trusted proxy host (the app then trusts no proxy)", func() {
		c := valid
		c.TrustedProxyHost = ""
		Expect(c.Validate()).To(Succeed())
	})

	It("scopes the cache to the watched namespace, plus the operator namespace for DiscordChannels", func() {
		opts := valid.CacheOptions()
		Expect(opts.DefaultNamespaces).To(HaveLen(1))
		Expect(opts.DefaultNamespaces).To(HaveKey("shops"))
		for obj, bo := range opts.ByObject {
			switch obj.(type) {
			case *corev1.Secret:
				Expect(bo.Label.String()).To(Equal(LabelManagedBy + "=" + ManagedByValue))
				Expect(bo.Namespaces).To(HaveLen(2))
				Expect(bo.Namespaces).To(HaveKey("shop-operator-system"))
			case *shophubv1alpha1.DiscordChannel:
				Expect(bo.Namespaces).To(HaveLen(2))
				Expect(bo.Namespaces).To(HaveKey("shop-operator-system"))
			default:
				Fail("unexpected ByObject entry")
			}
		}
		Expect(opts.ByObject).To(HaveLen(2))
	})
})

var _ = Describe("DetectDatabaseKinds (research R5)", func() {
	It("reports both kinds when the upstream CRDs are installed", func() {
		k, err := DetectDatabaseKinds(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(k).To(Equal(DatabaseKinds{Postgres: true, Redis: true}))
	})

	It("reports neither kind, without error, on a cluster without the database operators", func() {
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: testEnv.BinaryAssetsDirectory,
		}
		bareCfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(env.Stop)

		k, err := DetectDatabaseKinds(bareCfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(k).To(Equal(DatabaseKinds{}))
	})
})

var _ = Describe("SetupIndexes (research R13)", func() {
	It("finds Shops by walletRef and discordChannelRef through the cache", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())
		mctx, mcancel := context.WithCancel(ctx)
		DeferCleanup(mcancel)
		Expect(SetupIndexes(mctx, mgr)).To(Succeed())
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()

		shop := &shophubv1alpha1.Shop{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName("idx"), Namespace: "default"},
			Spec: shophubv1alpha1.ShopSpec{
				ShopName: "Idx", Availability: shophubv1alpha1.AvailabilityStandard,
				Database:          shophubv1alpha1.DatabaseStandard,
				WalletRef:         shophubv1alpha1.LocalObjectReference{Name: "idx-wallet"},
				DiscordChannelRef: shophubv1alpha1.LocalObjectReference{Name: "idx-channel"},
			},
		}
		Expect(k8sClient.Create(ctx, shop)).To(Succeed())

		for field, value := range map[string]string{
			IndexShopWalletRef:         "idx-wallet",
			IndexShopDiscordChannelRef: "idx-channel",
		} {
			Eventually(func(g Gomega) {
				var list shophubv1alpha1.ShopList
				g.Expect(mgr.GetClient().List(ctx, &list, client.InNamespace("default"),
					client.MatchingFields{field: value})).To(Succeed())
				g.Expect(list.Items).To(HaveLen(1))
				g.Expect(list.Items[0].Name).To(Equal(shop.Name))
			}, "10s").Should(Succeed())
		}
	})
})
