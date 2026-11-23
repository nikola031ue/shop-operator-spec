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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// SC-005: every not-ready reason must show within 1 minute.
const reasonTimeout = "60s"

// testShopName is the shop name used across tests.
const testShopName = "acme"

// testOwner is the shophub.io/owner label value used across tests.
const testOwner = "user-42"

// fieldSpec is the unstructured "spec" key used in test fixtures.
const fieldSpec = "spec"

// testImageTag is the pinned placeholder image tag used across tests.
const testImageTag = "1.27.3"

func keyOf(o client.Object) client.ObjectKey { return client.ObjectKeyFromObject(o) }

// newNamespace creates a throwaway namespace so tests don't see each other's objects.
func newNamespace() string {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// readyOf returns "<status>/<reason>" of the Ready condition, or "" if absent.
func readyOf(conds []metav1.Condition) string {
	c := meta.FindStatusCondition(conds, ConditionReady)
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s", c.Status, c.Reason)
}

func newWallet(ns, name, address string) *shophubv1alpha1.Wallet {
	return &shophubv1alpha1.Wallet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels: map[string]string{LabelOwner: testOwner, LabelShop: testShopName}},
		Spec: shophubv1alpha1.WalletSpec{Address: address},
	}
}

func newDiscordChannel(ns, name string) *shophubv1alpha1.DiscordChannel {
	return &shophubv1alpha1.DiscordChannel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       shophubv1alpha1.DiscordChannelSpec{ChannelName: name},
	}
}

func newShop(ns, name, walletName string, db shophubv1alpha1.DatabaseType) *shophubv1alpha1.Shop {
	return &shophubv1alpha1.Shop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels: map[string]string{LabelOwner: testOwner, LabelShop: name}},
		Spec: shophubv1alpha1.ShopSpec{
			ShopName: "Shop " + name, Availability: shophubv1alpha1.AvailabilityStandard, Database: db,
			WalletRef:         shophubv1alpha1.LocalObjectReference{Name: walletName},
			DiscordChannelRef: shophubv1alpha1.LocalObjectReference{Name: "ch"},
		},
	}
}

// getWallet reads a Wallet for assertions inside Eventually.
func getWallet(g Gomega, w *shophubv1alpha1.Wallet) *shophubv1alpha1.Wallet {
	var got shophubv1alpha1.Wallet
	g.Expect(k8sClient.Get(ctx, keyOf(w), &got)).To(Succeed())
	return &got
}
