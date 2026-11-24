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

// Package resources builds the desired child objects of a Shop (research R11, data-model.md
// "Owns"). Builders are pure: no API calls, so they are unit-tested in isolation. Every object
// has TypeMeta set so the reconciler can server-side apply it.
package resources

import (
	"errors"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

const (
	// RedisImage is the pinned Redis server image for `light` shops (never `latest`).
	RedisImage = "quay.io/opstree/redis:v7.2.16"
	// HTTPPort is the port the shop application listens on.
	HTTPPort = 80
	// LabelName / LabelInstance select a shop's pods.
	LabelName     = "app.kubernetes.io/name"
	LabelInstance = "app.kubernetes.io/instance"
)

// Child names (data-model.md "Owns").
func DeploymentName(shop *shophubv1alpha1.Shop) string  { return shop.Name }
func ServiceName(shop *shophubv1alpha1.Shop) string     { return shop.Name }
func CNPGClusterName(shop *shophubv1alpha1.Shop) string { return shop.Name + "-db" }
func RedisName(shop *shophubv1alpha1.Shop) string       { return shop.Name + "-redis" }

// CNPGAppSecretName is the Secret CNPG generates for the app user; key "uri" holds the URL.
func CNPGAppSecretName(shop *shophubv1alpha1.Shop) string { return CNPGClusterName(shop) + "-app" }

// URL is the in-cluster address of a shop (FR-008).
func URL(shop *shophubv1alpha1.Shop) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", ServiceName(shop), shop.Namespace)
}

// ErrUnpinnedImage is returned for an empty or `latest` image tag (FR-007, Principle II).
var ErrUnpinnedImage = errors.New("image tag must be pinned (not empty, not latest)")

// ImageRef joins repository and tag, refusing unpinned tags.
func ImageRef(repository, tag string) (string, error) {
	if repository == "" || tag == "" || tag == "latest" {
		return "", ErrUnpinnedImage
	}
	return repository + ":" + tag, nil
}

// ShopParams is everything the application builders need.
type ShopParams struct {
	Shop *shophubv1alpha1.Shop
	// Image is a pinned repository:tag from ImageRef.
	Image string
	// WalletAddress is the payment address from the referenced Wallet's status.
	WalletAddress string
	// Labels are the child labels (managed-by + owner labels copied from the Shop).
	Labels map[string]string
	// AdminSecretUID / RedisSecretUID are the UIDs of the generated password Secrets. They go
	// into pod-template annotations so a recreated Secret rolls the pods (003 contract). UIDs
	// only, never values.
	AdminSecretUID string
	RedisSecretUID string
	// RedisSecretName is the light Shop's current Redis password Secret; empty means
	// RedisAuthSecretName (operator 0.2.1 switches to a new name when the Secret is lost).
	RedisSecretName string
}

// redisSecretName returns the Redis password Secret to reference.
func (p ShopParams) redisSecretName() string {
	if p.RedisSecretName != "" {
		return p.RedisSecretName
	}
	return RedisAuthSecretName(p.Shop)
}

// Pod-template annotations carrying Secret UIDs (003 contract).
const (
	AnnotationAdminSecretUID = "shophub.io/admin-secret-uid"
	AnnotationRedisSecretUID = "shophub.io/redis-secret-uid"
)

// Shop container resources (003 research R16): the app must not depend on fitting 128Mi.
var (
	shopCPURequest    = resource.MustParse("10m")
	shopMemoryRequest = resource.MustParse("128Mi")
	shopMemoryLimit   = resource.MustParse("256Mi")
)

// Unstructured field names used more than once.
const (
	keySpec    = "spec"
	keyStorage = "storage"
)

// RedisStorageSize is the PVC size of a light shop's Redis (003 research R3).
const RedisStorageSize = "1Gi"

func secretEnv(name, secret string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: PasswordKey,
	}}}
}

func selectorLabels(shop *shophubv1alpha1.Shop) map[string]string {
	return map[string]string{LabelName: "shop", LabelInstance: shop.Name}
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

// Deployment builds the shop application Deployment (FR-002, FR-004, FR-007).
func Deployment(p ShopParams) *appsv1.Deployment {
	shop := p.Shop
	replicas := shop.Spec.Availability.Replicas()
	env := []corev1.EnvVar{
		{Name: "SHOP_NAME", Value: shop.Spec.ShopName},
		{Name: "WALLET_ADDRESS", Value: p.WalletAddress},
		secretEnv("ADMIN_PASSWORD", AdminSecretName(shop)),
	}
	annotations := map[string]string{AnnotationAdminSecretUID: p.AdminSecretUID}
	switch shop.Spec.Database {
	case shophubv1alpha1.DatabaseLight:
		env = append(env, corev1.EnvVar{
			Name:  "REDIS_URL",
			Value: fmt.Sprintf("redis://%s.%s.svc:6379", RedisName(shop), shop.Namespace),
		}, secretEnv("REDIS_PASSWORD", p.redisSecretName()))
		annotations[AnnotationRedisSecretUID] = p.RedisSecretUID
	default:
		env = append(env, corev1.EnvVar{
			Name: "DATABASE_URL",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: CNPGAppSecretName(shop)},
				Key:                  "uri",
			}},
		})
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: DeploymentName(shop), Namespace: shop.Namespace,
			Labels: merge(p.Labels, selectorLabels(shop)),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(shop)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      merge(p.Labels, selectorLabels(shop)),
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					// Lets a non-root app bind port 80; a namespaced "safe" sysctl (003 R5).
					SecurityContext: &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{
						{Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"},
					}},
					Containers: []corev1.Container{{
						Name:  "shop",
						Image: p.Image,
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: HTTPPort, Protocol: corev1.ProtocolTCP}},
						Env:   env,
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")},
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    shopCPURequest,
								corev1.ResourceMemory: shopMemoryRequest,
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: shopMemoryLimit},
						},
					}},
				},
			},
		},
	}
}

// Service builds the ClusterIP Service in front of the shop pods (FR-008).
func Service(p ShopParams) *corev1.Service {
	shop := p.Shop
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name: ServiceName(shop), Namespace: shop.Namespace,
			Labels: merge(p.Labels, selectorLabels(shop)),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(shop),
			Ports: []corev1.ServicePort{{
				Name: "http", Port: HTTPPort, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// CNPGCluster builds a one-instance CloudNativePG Cluster for a `standard` shop (FR-003).
// CNPG creates database "app" and Secret <cluster>-app with the connection URI.
func CNPGCluster(shop *shophubv1alpha1.Shop, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		keySpec: map[string]any{
			"instances": int64(1),
			keyStorage:  map[string]any{"size": "1Gi"},
			"bootstrap": map[string]any{"initdb": map[string]any{"database": "app", "owner": "app"}},
		},
	}}
	u.SetAPIVersion("postgresql.cnpg.io/v1")
	u.SetKind("Cluster")
	u.SetName(CNPGClusterName(shop))
	u.SetNamespace(shop.Namespace)
	u.SetLabels(merge(labels))
	return u
}

// Redis builds a standalone OT Redis for a `light` shop (FR-003). It requires the password in
// Secret secretName (003 R2; RedisAuthSecretName or a later generation) and keeps its data on a
// PVC with AOF enabled (003 R3). Changing secretName makes the Redis operator roll the pod with
// the new password and the same PVC (operator 0.2.1). The Redis operator removes the PVC when
// the Redis is deleted (keepAfterDelete: false), and the Redis is owned by the Shop.
//
// PVC labels cannot be set here: the OT CRD declares volumeClaimTemplate.metadata without
// properties, so the API server prunes them. The Redis CR itself carries the Shop labels.
func Redis(shop *shophubv1alpha1.Shop, labels map[string]string, secretName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		keySpec: map[string]any{
			"kubernetesConfig": map[string]any{
				"image":           RedisImage,
				"imagePullPolicy": "IfNotPresent",
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
					"limits":   map[string]any{"memory": "128Mi"},
				},
				"redisSecret": map[string]any{"name": secretName, "key": PasswordKey},
			},
			keyStorage: map[string]any{
				"keepAfterDelete": false,
				"volumeClaimTemplate": map[string]any{
					keySpec: map[string]any{
						"accessModes": []any{"ReadWriteOnce"},
						"resources":   map[string]any{"requests": map[string]any{keyStorage: RedisStorageSize}},
					},
				},
			},
			"redisConfig": map[string]any{
				"dynamicConfig": []any{"appendonly yes"},
			},
			"podSecurityContext": map[string]any{"runAsUser": int64(1000), "fsGroup": int64(1000)},
		},
	}}
	u.SetAPIVersion("redis.redis.opstreelabs.in/v1beta2")
	u.SetKind("Redis")
	u.SetName(RedisName(shop))
	u.SetNamespace(shop.Namespace)
	u.SetLabels(merge(labels))
	return u
}
