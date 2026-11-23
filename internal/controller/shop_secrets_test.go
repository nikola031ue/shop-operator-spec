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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/resources"
)

// Operator 0.2.0: generated passwords, Redis auth/storage and the 0.1.0 upgrade
// (spec 003 contracts/operator-0.2.0.md, research R1–R5, R16).

func getSecret(g Gomega, ns, name string) *corev1.Secret {
	var s corev1.Secret
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s)).To(Succeed())
	return &s
}

func getRedis(g Gomega, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(RedisGVK)
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u)).To(Succeed())
	return u
}

var _ = Describe("Shop reconciler: operator 0.2.0 (spec 003)", func() {
	var ns string

	setupRefs := func() {
		Expect(k8sClient.Create(ctx, newWallet(ns, "w", addrA))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDiscordChannel(ns, "ch"))).To(Succeed())
		markChannelReady(k8sClient, ns, "ch")
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getWallet(g, newWallet(ns, "w", "")).Status.Conditions)).To(Equal("True/Ready"))
		}, reasonTimeout).Should(Succeed())
	}

	BeforeEach(func() { ns = newNamespace() })

	It("gives every Shop an owned admin password Secret passed by reference (R1)", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, testShopName, "w", shophubv1alpha1.DatabaseStandard))).To(Succeed())

		var sec *corev1.Secret
		Eventually(func(g Gomega) {
			sec = getSecret(g, ns, "acme-admin")
			d := getDeployment(g, ns, testShopName)
			g.Expect(d.Spec.Template.Annotations).To(HaveKeyWithValue(resources.AnnotationAdminSecretUID, string(sec.UID)))
		}, reasonTimeout).Should(Succeed())

		pw := string(sec.Data[resources.PasswordKey])
		Expect(pw).To(HaveLen(resources.AdminPasswordLength))
		Expect(sec.Labels).To(HaveKeyWithValue(LabelManagedBy, ManagedByValue))
		Expect(sec.Labels).To(HaveKeyWithValue(LabelOwner, testOwner))
		Expect(sec.Labels).To(HaveKeyWithValue(LabelShop, testShopName))
		expectControlledBy(sec, getShop(Default, ns, testShopName))

		d := getDeployment(Default, ns, testShopName)
		env := envOf(d)
		Expect(env["ADMIN_PASSWORD"].Value).To(BeEmpty())
		Expect(env["ADMIN_PASSWORD"].ValueFrom.SecretKeyRef.Name).To(Equal("acme-admin"))
		Expect(env["ADMIN_PASSWORD"].ValueFrom.SecretKeyRef.Key).To(Equal(resources.PasswordKey))
		Expect(env).NotTo(HaveKey("REDIS_PASSWORD"))
		Expect(d.Spec.Template.Spec.SecurityContext.Sysctls).To(ContainElement(corev1.Sysctl{
			Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"}))
		res := d.Spec.Template.Spec.Containers[0].Resources
		Expect(res.Requests.Memory().String()).To(Equal("128Mi"))
		Expect(res.Limits.Memory().String()).To(Equal("256Mi"))

		By("never touching an existing password on later reconciles")
		Consistently(func(g Gomega) {
			g.Expect(string(getSecret(g, ns, "acme-admin").Data[resources.PasswordKey])).To(Equal(pw))
		}, "3s").Should(Succeed())

		By("regenerating a deleted admin Secret and rolling the pods via the UID annotation")
		Expect(k8sClient.Delete(ctx, sec)).To(Succeed())
		Eventually(func(g Gomega) {
			again := getSecret(g, ns, "acme-admin")
			g.Expect(again.UID).NotTo(Equal(sec.UID))
			g.Expect(getDeployment(g, ns, testShopName).Spec.Template.Annotations).
				To(HaveKeyWithValue(resources.AnnotationAdminSecretUID, string(again.UID)))
		}, reasonTimeout).Should(Succeed())

		By("never logging the password (SC-007)")
		Expect(logBuf.String()).NotTo(ContainSubstring(pw))
	})

	It("protects light-shop Redis with a password and gives it persistent storage (R2, R3)", func() {
		setupRefs()
		Expect(k8sClient.Create(ctx, newShop(ns, "lite", "w", shophubv1alpha1.DatabaseLight))).To(Succeed())

		var sec *corev1.Secret
		Eventually(func(g Gomega) {
			sec = getSecret(g, ns, "lite-redis-auth")
			g.Expect(getDeployment(g, ns, "lite").Spec.Template.Annotations).
				To(HaveKeyWithValue(resources.AnnotationRedisSecretUID, string(sec.UID)))
		}, reasonTimeout).Should(Succeed())
		Expect(sec.Data[resources.PasswordKey]).To(HaveLen(resources.RedisPasswordLength))
		expectControlledBy(sec, getShop(Default, ns, "lite"))

		env := envOf(getDeployment(Default, ns, "lite"))
		Expect(env["REDIS_PASSWORD"].ValueFrom.SecretKeyRef.Name).To(Equal("lite-redis-auth"))
		Expect(env["REDIS_URL"].Value).To(Equal("redis://lite-redis." + ns + ".svc:6379"))

		r := getRedis(Default, ns, "lite-redis")
		name, _, _ := unstructured.NestedString(r.Object, "spec", "kubernetesConfig", "redisSecret", "name")
		Expect(name).To(Equal("lite-redis-auth"))
		size, _, _ := unstructured.NestedString(r.Object, "spec", "storage", "volumeClaimTemplate", "spec", "resources", "requests", "storage")
		Expect(size).To(Equal("1Gi"))
		Expect(r.GetLabels()).To(HaveKeyWithValue(LabelShop, "lite")) // PVC labels: see resources.Redis
		dyn, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "redisConfig", "dynamicConfig")
		Expect(dyn).To(ConsistOf("appendonly yes"))

		By("reporting RedisCredentialsLost, not regenerating, when the Redis Secret is deleted")
		Expect(k8sClient.Delete(ctx, sec)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(readyOf(getShop(g, ns, "lite").Status.Conditions)).To(Equal("False/" + ReasonRedisCredentialsLost))
		}, reasonTimeout).Should(Succeed())
		Consistently(func() bool {
			var s corev1.Secret
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "lite-redis-auth"}, &s))
		}, "3s").Should(BeTrue())
		Expect(logBuf.String()).NotTo(ContainSubstring(string(sec.Data[resources.PasswordKey])))
	})

	It("recreates a 0.1.0 Redis without storage once, with no hand change (R4)", func() {
		setupRefs()
		By("simulating a Redis created by operator 0.1.0 (no password, no storage)")
		old := &unstructured.Unstructured{Object: map[string]any{fieldSpec: map[string]any{
			"kubernetesConfig": map[string]any{"image": resources.RedisImage},
		}}}
		old.SetGroupVersionKind(RedisGVK)
		old.SetNamespace(ns)
		old.SetName("old-redis")
		Expect(k8sClient.Create(ctx, old)).To(Succeed())

		Expect(k8sClient.Create(ctx, newShop(ns, "old", "w", shophubv1alpha1.DatabaseLight))).To(Succeed())
		Eventually(func(g Gomega) {
			r := getRedis(g, ns, "old-redis")
			g.Expect(r.GetUID()).NotTo(Equal(old.GetUID()))
			_, found, _ := unstructured.NestedMap(r.Object, "spec", "storage")
			g.Expect(found).To(BeTrue())
			g.Expect(getSecret(g, ns, "old-redis-auth")).NotTo(BeNil())
			g.Expect(readyOf(getShop(g, ns, "old").Status.Conditions)).To(Equal("False/" + ReasonDatabaseProvisioning))
		}, reasonTimeout).Should(Succeed())

		By("leaving the new Redis alone afterwards")
		recreated := getRedis(Default, ns, "old-redis")
		Consistently(func(g Gomega) {
			g.Expect(getRedis(g, ns, "old-redis").GetUID()).To(Equal(recreated.GetUID()))
		}, "3s").Should(Succeed())
		Expect(logBuf.String()).To(ContainSubstring("Recreating Redis without storage (0.1.0 upgrade)"))
	})
})
