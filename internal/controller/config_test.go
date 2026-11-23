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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

var _ = Describe("Config.Validate (FR-007)", func() {
	valid := Config{
		ShopImage: "nginx", ShopImageTag: "1.27.3",
		DiscordTokenSecret: types.NamespacedName{Namespace: "shop-operator-system", Name: "shop-operator-discord"},
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
	)
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
