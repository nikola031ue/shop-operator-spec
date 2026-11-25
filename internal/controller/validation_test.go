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
	"slices"
	"strings"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Admission rejections from contracts/crds.md (SC-004). Unstructured objects let a test omit
// a field entirely, which a typed client cannot do.

var nameSeq atomic.Int64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nameSeq.Add(1))
}

func newObj(kind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{fieldSpec: spec}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "shophub.io", Version: "v1alpha1", Kind: kind})
	u.SetNamespace("default")
	u.SetName(name)
	return u
}

func validShopSpec() map[string]any {
	return map[string]any{
		"shopName":          "Acme Clothing",
		"availability":      "standard",
		"database":          "standard",
		"walletRef":         map[string]any{"name": "w"},
		"discordChannelRef": map[string]any{"name": "d"},
	}
}

// patchSpec sends a JSON merge patch setting spec.<path> to value (nil removes the field).
// A merge patch carries no resourceVersion, so it never conflicts with the reconcilers that
// update the same object's status and finalizers.
func patchSpec(obj *unstructured.Unstructured, value any, path ...string) error {
	node := value
	for _, p := range slices.Backward(path) {
		node = map[string]any{p: node}
	}
	raw, err := json.Marshal(map[string]any{fieldSpec: node})
	if err != nil {
		return err
	}
	return k8sClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
}

func expectRejected(err error, fragments ...string) {
	GinkgoHelper()
	Expect(err).To(HaveOccurred(), "expected the API server to reject the object")
	for _, f := range fragments {
		Expect(err.Error()).To(ContainSubstring(f))
	}
}

var _ = Describe("CRD admission validation (contracts/crds.md, SC-004)", func() {
	Context("Shop", func() {
		It("accepts a valid Shop", func() {
			Expect(k8sClient.Create(ctx, newObj("Shop", uniqueName("ok"), validShopSpec()))).To(Succeed())
		})

		It("rejects an unknown availability and lists the allowed values", func() {
			spec := validShopSpec()
			spec["availability"] = "medium"
			err := k8sClient.Create(ctx, newObj("Shop", uniqueName("avail"), spec))
			expectRejected(err, `Unsupported value: "medium"`, `"standard"`, `"high"`)
		})

		It("rejects an unknown database type", func() {
			spec := validShopSpec()
			spec["database"] = "mongo"
			expectRejected(k8sClient.Create(ctx, newObj("Shop", uniqueName("db"), spec)), `Unsupported value: "mongo"`)
		})

		for _, field := range []string{"walletRef", "discordChannelRef", "shopName", "availability", "database"} {
			It("rejects a Shop without "+field, func() {
				spec := validShopSpec()
				delete(spec, field)
				err := k8sClient.Create(ctx, newObj("Shop", uniqueName("missing"), spec))
				expectRejected(err, "Required value", "spec."+field)
			})
		}

		It("rejects a name longer than 40 characters", func() {
			err := k8sClient.Create(ctx, newObj("Shop", "a"+strings.Repeat("b", 40), validShopSpec()))
			expectRejected(err, "metadata.name must be at most 40 characters")
		})

		// The host <name>.<shop-domain> must stay one label under the wildcard certificate.
		It("rejects a name containing a dot", func() {
			err := k8sClient.Create(ctx, newObj("Shop", "acme.shop", validShopSpec()))
			expectRejected(err, "metadata.name must not contain dots")
		})

		It("rejects changing database or shopName", func() {
			obj := newObj("Shop", uniqueName("immut"), validShopSpec())
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			expectRejected(patchSpec(obj, "light", "database"), "database is immutable")
			expectRejected(patchSpec(obj, "Other name", "shopName"), "shopName is immutable")
		})

		It("allows changing availability and walletRef", func() {
			obj := newObj("Shop", uniqueName("mut"), validShopSpec())
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			Expect(patchSpec(obj, "high", "availability")).To(Succeed())
			Expect(patchSpec(obj, "w2", "walletRef", "name")).To(Succeed())
		})
	})

	Context("Wallet", func() {
		It("accepts a Wallet without address and one with a valid address", func() {
			Expect(k8sClient.Create(ctx, newObj("Wallet", uniqueName("gen"), map[string]any{}))).To(Succeed())
			Expect(k8sClient.Create(ctx, newObj("Wallet", uniqueName("reg"), map[string]any{
				"address": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
			}))).To(Succeed())
		})

		It("rejects a malformed address", func() {
			err := k8sClient.Create(ctx, newObj("Wallet", uniqueName("bad"), map[string]any{"address": "0x123"}))
			expectRejected(err, "should match '^0x[0-9a-fA-F]{40}$'")
		})

		It("allows setting an address once, then rejects changing or removing it", func() {
			obj := newObj("Wallet", uniqueName("once"), map[string]any{})
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())

			Expect(patchSpec(obj, "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", "address")).To(Succeed())
			expectRejected(patchSpec(obj, "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359", "address"), "address is immutable")
			expectRejected(patchSpec(obj, nil, "address"), "address is immutable")
		})
	})

	Context("DiscordChannel", func() {
		It("accepts a valid channel name", func() {
			Expect(k8sClient.Create(ctx, newObj("DiscordChannel", uniqueName("dch"), map[string]any{
				"channelName": "acme-alerts",
			}))).To(Succeed())
		})

		It("rejects an invalid channel name", func() {
			err := k8sClient.Create(ctx, newObj("DiscordChannel", uniqueName("dch"), map[string]any{
				"channelName": "Acme Alerts",
			}))
			expectRejected(err, "should match '^[a-z0-9-]{1,100}$'")
		})

		It("rejects a DiscordChannel without channelName", func() {
			err := k8sClient.Create(ctx, newObj("DiscordChannel", uniqueName("dch"), map[string]any{}))
			expectRejected(err, "Required value", "spec.channelName")
		})
	})
})
