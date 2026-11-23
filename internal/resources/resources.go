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
	}
	switch shop.Spec.Database {
	case shophubv1alpha1.DatabaseLight:
		env = append(env, corev1.EnvVar{
			Name:  "REDIS_URL",
			Value: fmt.Sprintf("redis://%s.%s.svc:6379", RedisName(shop), shop.Namespace),
		})
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
				ObjectMeta: metav1.ObjectMeta{Labels: merge(p.Labels, selectorLabels(shop))},
				Spec: corev1.PodSpec{
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
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
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
		"spec": map[string]any{
			"instances": int64(1),
			"storage":   map[string]any{"size": "1Gi"},
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

// Redis builds a standalone OT Redis for a `light` shop (FR-003). No password on the local
// cluster (research R11 note).
func Redis(shop *shophubv1alpha1.Shop, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"kubernetesConfig": map[string]any{
				"image":           RedisImage,
				"imagePullPolicy": "IfNotPresent",
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
					"limits":   map[string]any{"memory": "128Mi"},
				},
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
