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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// IngressName / NetworkPolicyName are the per-Shop routing children (spec 004 data-model.md
// "Derived resources added by shop-operator 0.3.0").
func IngressName(shop *shophubv1alpha1.Shop) string       { return shop.Name }
func NetworkPolicyName(shop *shophubv1alpha1.Shop) string { return shop.Name }

// LabelNamespaceName is the label the API server sets on every namespace to its own name.
const LabelNamespaceName = "kubernetes.io/metadata.name"

// Host is a shop's public host name: <shop>.<shop-domain> (spec 004 FR-021).
func Host(shop *shophubv1alpha1.Shop, domain string) string { return shop.Name + "." + domain }

// URL is a shop's public HTTPS address through the platform entry point (spec 004 FR-021). The
// entry point's default certificate covers *.<shop-domain>.
func URL(shop *shophubv1alpha1.Shop, domain string) string { return "https://" + Host(shop, domain) }

// Ingress routes https://<shop>.<shop-domain>/ to the shop Service. It has no tls block: the
// entry point's default certificate covers the host (spec 004 contracts/platform-phase1.md).
func Ingress(p ShopParams) *networkingv1.Ingress {
	shop := p.Shop
	prefix := networkingv1.PathTypePrefix
	class := p.IngressClass
	return &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Name: IngressName(shop), Namespace: shop.Namespace,
			Labels: merge(p.Labels, selectorLabels(shop)),
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			Rules: []networkingv1.IngressRule{{
				Host: Host(shop, p.ShopDomain),
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &prefix,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: ServiceName(shop), Port: networkingv1.ServiceBackendPort{Number: HTTPPort},
						}},
					}},
				}},
			}},
		},
	}
}

// NetworkPolicy admits traffic to the shop pods, on the app port, only from the entry point's
// namespace and from the ShopHub app pods (for the FR-016 hand-off status read), and on the
// internal port only from the operator's pods (spec 005). Forwarded
// headers are trusted from Traefik alone, so ShopHub is an untrusted peer (spec 004 research R5).
func NetworkPolicy(p ShopParams) *networkingv1.NetworkPolicy {
	shop := p.Shop
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(HTTPPort)
	from := func(ns string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{LabelNamespaceName: ns},
		}}
	}
	// Namespace and pod selector in one peer: both must match.
	shophub := from(p.ShopHubNamespace)
	shophub.PodSelector = &metav1.LabelSelector{MatchLabels: p.ShopHubPodLabels}
	operator := from(p.OperatorNamespace)
	operator.PodSelector = &metav1.LabelSelector{MatchLabels: p.OperatorPodLabels}
	internal := intstr.FromInt32(InternalPort)
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: NetworkPolicyName(shop), Namespace: shop.Namespace,
			Labels: merge(p.Labels, selectorLabels(shop)),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorLabels(shop)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{from(p.IngressNamespace), shophub},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
				},
				// Spec 005 research R4: the internal API port only from the operator's own pods.
				{
					From:  []networkingv1.NetworkPolicyPeer{operator},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &internal}},
				},
			},
		},
	}
}
