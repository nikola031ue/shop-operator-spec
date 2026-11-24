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

package resources

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

const (
	tShop     = "acme"
	tImage    = "nginx:1.27.3"
	tOwner    = "user-42"
	tAdminUID = "uid-admin"
)

func shop(avail shophubv1alpha1.Availability, db shophubv1alpha1.DatabaseType) *shophubv1alpha1.Shop {
	return &shophubv1alpha1.Shop{
		ObjectMeta: metav1.ObjectMeta{Name: tShop, Namespace: tOwner},
		Spec: shophubv1alpha1.ShopSpec{
			ShopName: "Acme Clothing", Availability: avail, Database: db,
			WalletRef:         shophubv1alpha1.LocalObjectReference{Name: "w"},
			DiscordChannelRef: shophubv1alpha1.LocalObjectReference{Name: "d"},
		},
	}
}

var owner = map[string]string{
	"app.kubernetes.io/managed-by": "shop-operator", "shophub.io/owner": tOwner, "shophub.io/shop": tShop,
}

func params(s *shophubv1alpha1.Shop) ShopParams {
	return ShopParams{Shop: s, Image: tImage, WalletAddress: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Labels: owner}
}

func envMap(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestImageRefNeverLatest(t *testing.T) {
	if got, err := ImageRef("nginx", "1.27.3"); err != nil || got != tImage {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, tc := range [][2]string{{"nginx", "latest"}, {"nginx", ""}, {"", "1.0.0"}} {
		if _, err := ImageRef(tc[0], tc[1]); !errors.Is(err, ErrUnpinnedImage) {
			t.Fatalf("%v: want ErrUnpinnedImage, got %v", tc, err)
		}
	}
	if strings.HasSuffix(RedisImage, ":latest") || !strings.Contains(RedisImage, ":v") {
		t.Fatalf("RedisImage must be pinned, got %q", RedisImage)
	}
}

func TestDeploymentReplicasFollowAvailability(t *testing.T) {
	for avail, want := range map[shophubv1alpha1.Availability]int32{
		shophubv1alpha1.AvailabilityStandard: 2, shophubv1alpha1.AvailabilityHigh: 3,
	} {
		d := Deployment(params(shop(avail, shophubv1alpha1.DatabaseStandard)))
		if *d.Spec.Replicas != want {
			t.Fatalf("%s: replicas %d, want %d", avail, *d.Spec.Replicas, want)
		}
	}
}

func TestDeploymentStandardEnv(t *testing.T) {
	d := Deployment(params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard)))
	if d.Name != tShop || d.Namespace != tOwner || d.Kind != "Deployment" || d.APIVersion != "apps/v1" {
		t.Fatalf("unexpected identity %s/%s %s %s", d.Namespace, d.Name, d.APIVersion, d.Kind)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != tImage {
		t.Fatalf("image %q", c.Image)
	}
	env := envMap(c)
	if env["SHOP_NAME"].Value != "Acme Clothing" || env["WALLET_ADDRESS"].Value != "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed" {
		t.Fatalf("env %+v", env)
	}
	ref := env["DATABASE_URL"].ValueFrom
	if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "acme-db-app" || ref.SecretKeyRef.Key != "uri" {
		t.Fatalf("DATABASE_URL must come from secret acme-db-app/uri, got %+v", env["DATABASE_URL"])
	}
	if env["DATABASE_URL"].Value != "" {
		t.Fatal("DATABASE_URL must not carry a literal value (Principle VI)")
	}
	if _, ok := env["REDIS_URL"]; ok {
		t.Fatal("standard shop must not get REDIS_URL")
	}
}

func TestDeploymentLightEnv(t *testing.T) {
	d := Deployment(params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight)))
	env := envMap(d.Spec.Template.Spec.Containers[0])
	if env["REDIS_URL"].Value != "redis://acme-redis.user-42.svc:6379" {
		t.Fatalf("REDIS_URL %q", env["REDIS_URL"].Value)
	}
	if _, ok := env["DATABASE_URL"]; ok {
		t.Fatal("light shop must not get DATABASE_URL")
	}
}

func TestWalletChangeChangesPodTemplate(t *testing.T) {
	s := shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard)
	a := Deployment(params(s))
	p := params(s)
	p.WalletAddress = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"
	b := Deployment(p)
	if envMap(a.Spec.Template.Spec.Containers[0])["WALLET_ADDRESS"] == envMap(b.Spec.Template.Spec.Containers[0])["WALLET_ADDRESS"] {
		t.Fatal("a new wallet address must change the pod template so pods roll (FR-005)")
	}
}

func TestLabelsAndSelector(t *testing.T) {
	p := params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard))
	d, svc := Deployment(p), Service(p)
	for k, v := range owner {
		for name, got := range map[string]map[string]string{
			"deployment": d.Labels, "pod template": d.Spec.Template.Labels, "service": svc.Labels,
		} {
			if got[k] != v {
				t.Fatalf("%s label %s=%q, want %q", name, k, got[k], v)
			}
		}
	}
	sel := d.Spec.Selector.MatchLabels
	if len(sel) != 2 || sel[LabelInstance] != tShop || svc.Spec.Selector[LabelInstance] != tShop {
		t.Fatalf("selector must be the stable name/instance pair, got %v / %v", sel, svc.Spec.Selector)
	}
	for k, v := range sel {
		if d.Spec.Template.Labels[k] != v {
			t.Fatalf("pod template must carry selector label %s", k)
		}
	}
	p.Labels["shophub.io/owner"] = "changed"
	if Deployment(p).Spec.Selector.MatchLabels["shophub.io/owner"] != "" {
		t.Fatal("owner labels must not be part of the (immutable) selector")
	}
}

func TestService(t *testing.T) {
	svc := Service(params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard)))
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].Port != 80 || svc.Kind != "Service" {
		t.Fatalf("unexpected service %+v", svc.Spec)
	}
}

func TestDatabaseBuilders(t *testing.T) {
	s := shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard)
	c := CNPGCluster(s, owner)
	if c.GetAPIVersion() != "postgresql.cnpg.io/v1" || c.GetKind() != "Cluster" || c.GetName() != "acme-db" || c.GetNamespace() != tOwner {
		t.Fatalf("cnpg identity %s %s %s/%s", c.GetAPIVersion(), c.GetKind(), c.GetNamespace(), c.GetName())
	}
	if n, _, _ := unstructured.NestedInt64(c.Object, "spec", "instances"); n != 1 {
		t.Fatalf("instances %d", n)
	}
	r := Redis(s, owner, RedisAuthSecretName(s))
	if r.GetAPIVersion() != "redis.redis.opstreelabs.in/v1beta2" || r.GetKind() != "Redis" || r.GetName() != "acme-redis" {
		t.Fatalf("redis identity %s %s %s", r.GetAPIVersion(), r.GetKind(), r.GetName())
	}
	if img, _, _ := unstructured.NestedString(r.Object, "spec", "kubernetesConfig", "image"); img != RedisImage {
		t.Fatalf("redis image %q", img)
	}
	for _, u := range []*unstructured.Unstructured{c, r} {
		if u.GetLabels()["shophub.io/shop"] != tShop {
			t.Fatalf("%s must carry owner labels", u.GetKind())
		}
	}
}

// --- operator 0.2.0 (spec 003 contracts/operator-0.2.0.md) ---

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []int{AdminPasswordLength, RedisPasswordLength} {
		for range 20 {
			p, err := RandomPassword(n)
			if err != nil {
				t.Fatal(err)
			}
			if len(p) != n {
				t.Fatalf("length %d, want %d", len(p), n)
			}
			if strings.Trim(p, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
				t.Fatalf("not URL-safe: %q", p)
			}
			if seen[p] {
				t.Fatal("RandomPassword repeated a value")
			}
			seen[p] = true
		}
	}
}

func TestPasswordSecret(t *testing.T) {
	s := shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight)
	if AdminSecretName(s) != "acme-admin" || RedisAuthSecretName(s) != tRedisAuth {
		t.Fatalf("names %q %q", AdminSecretName(s), RedisAuthSecretName(s))
	}
	sec := PasswordSecret(s, AdminSecretName(s), owner, "pw-value")
	if sec.Namespace != tOwner || sec.Kind != "Secret" || sec.Type != corev1.SecretTypeOpaque {
		t.Fatalf("identity %s/%s %s %s", sec.Namespace, sec.Name, sec.Kind, sec.Type)
	}
	if sec.StringData[PasswordKey] != "pw-value" {
		t.Fatal("password must be stored under key \"password\"")
	}
	for k, v := range owner {
		if sec.Labels[k] != v {
			t.Fatalf("label %s=%q, want %q (cache filter + Principle V)", k, sec.Labels[k], v)
		}
	}
	owner["extra"] = "x"
	defer delete(owner, "extra")
	if sec.Labels["extra"] != "" {
		t.Fatal("Secret labels must be a copy")
	}
}

func secretRef(t *testing.T, env map[string]corev1.EnvVar, name, secret string) {
	t.Helper()
	e, ok := env[name]
	if !ok {
		t.Fatalf("%s missing", name)
	}
	if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
		e.ValueFrom.SecretKeyRef.Name != secret || e.ValueFrom.SecretKeyRef.Key != PasswordKey {
		t.Fatalf("%s must be a secretKeyRef to %s/password, got %+v", name, secret, e)
	}
}

func TestDeploymentPasswordRefsAndAnnotations(t *testing.T) {
	std := params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard))
	std.AdminSecretUID = tAdminUID
	d := Deployment(std)
	env := envMap(d.Spec.Template.Spec.Containers[0])
	secretRef(t, env, "ADMIN_PASSWORD", "acme-admin")
	if _, ok := env["REDIS_PASSWORD"]; ok {
		t.Fatal("standard shop must not get REDIS_PASSWORD")
	}
	ann := d.Spec.Template.Annotations
	if ann[AnnotationAdminSecretUID] != tAdminUID || ann[AnnotationRedisSecretUID] != "" {
		t.Fatalf("annotations %v", ann)
	}

	light := params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight))
	light.AdminSecretUID, light.RedisSecretUID = tAdminUID, "uid-redis"
	d = Deployment(light)
	env = envMap(d.Spec.Template.Spec.Containers[0])
	secretRef(t, env, "ADMIN_PASSWORD", "acme-admin")
	secretRef(t, env, "REDIS_PASSWORD", tRedisAuth)
	if strings.Contains(env["REDIS_URL"].Value, "@") {
		t.Fatal("REDIS_URL must stay credential-free")
	}
	if d.Spec.Template.Annotations[AnnotationRedisSecretUID] != "uid-redis" {
		t.Fatalf("annotations %v", d.Spec.Template.Annotations)
	}

	light.RedisSecretUID = "uid-redis-2"
	if d.Spec.Template.Annotations[AnnotationRedisSecretUID] == Deployment(light).Spec.Template.Annotations[AnnotationRedisSecretUID] {
		t.Fatal("a recreated Secret must change the pod template so pods roll")
	}
}

func TestDeploymentSysctlAndMemory(t *testing.T) {
	d := Deployment(params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard)))
	sc := d.Spec.Template.Spec.SecurityContext
	if sc == nil || len(sc.Sysctls) != 1 || sc.Sysctls[0].Name != "net.ipv4.ip_unprivileged_port_start" || sc.Sysctls[0].Value != "0" {
		t.Fatalf("sysctls %+v", sc)
	}
	r := d.Spec.Template.Spec.Containers[0].Resources
	if r.Requests.Memory().String() != "128Mi" || r.Limits.Memory().String() != "256Mi" || r.Requests.Cpu().String() != "10m" {
		t.Fatalf("resources %+v", r)
	}
}

func TestRedisAuthStorageAndAOF(t *testing.T) {
	r := Redis(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight), owner, "acme-redis-auth-4")
	if n, _, _ := unstructured.NestedString(r.Object, "spec", "kubernetesConfig", "redisSecret", "name"); n != "acme-redis-auth-4" {
		t.Fatalf("redisSecret.name %q", n)
	}
	if k, _, _ := unstructured.NestedString(r.Object, "spec", "kubernetesConfig", "redisSecret", "key"); k != PasswordKey {
		t.Fatalf("redisSecret.key %q", k)
	}
	if keep, found, _ := unstructured.NestedBool(r.Object, "spec", "storage", "keepAfterDelete"); !found || keep {
		t.Fatal("storage.keepAfterDelete must be false so deleting the Shop removes the data")
	}
	if s, _, _ := unstructured.NestedString(r.Object, "spec", "storage", "volumeClaimTemplate", "spec", "resources", "requests", "storage"); s != "1Gi" {
		t.Fatalf("storage size %q", s)
	}
	modes, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "storage", "volumeClaimTemplate", "spec", "accessModes")
	if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		t.Fatalf("accessModes %v", modes)
	}
	dyn, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "redisConfig", "dynamicConfig")
	if len(dyn) != 1 || dyn[0] != "appendonly yes" {
		t.Fatalf("dynamicConfig %v", dyn)
	}
}

func TestRedisAuthSecretGenerations(t *testing.T) {
	s := shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight)
	for name, want := range map[string]int{
		tRedisAuth: 1, tRedisAuth2: 2, "acme-redis-auth-17": 17,
		"acme-redis-auth-1": 0, "acme-redis-auth-0": 0, "acme-redis-auth-02": 0, "acme-redis-auth-": 0,
		"acme-redis-auth-x": 0, "acme-redis-auth-2x": 0, "other-redis-auth": 0, "": 0,
	} {
		if got := RedisAuthSecretGeneration(s, name); got != want {
			t.Errorf("generation(%q) = %d, want %d", name, got, want)
		}
	}
	for cur, want := range map[string]string{
		tRedisAuth: tRedisAuth2, tRedisAuth2: "acme-redis-auth-3",
		"acme-redis-auth-9": "acme-redis-auth-10", "unrelated": tRedisAuth2,
	} {
		if got := NextRedisAuthSecretName(s, cur); got != want {
			t.Errorf("next(%q) = %q, want %q", cur, got, want)
		}
	}
}

func TestDeploymentFollowsRedisSecretName(t *testing.T) {
	p := params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseLight))
	p.RedisSecretName = "acme-redis-auth-3"
	secretRef(t, envMap(Deployment(p).Spec.Template.Spec.Containers[0]), "REDIS_PASSWORD", "acme-redis-auth-3")
}

const (
	tRedisAuth  = "acme-redis-auth"
	tRedisAuth2 = "acme-redis-auth-2"
)
