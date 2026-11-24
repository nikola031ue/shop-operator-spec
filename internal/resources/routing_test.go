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
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

const tTraefik = "traefik"

func routedParams() ShopParams {
	p := params(shop(shophubv1alpha1.AvailabilityStandard, shophubv1alpha1.DatabaseStandard))
	// A private copy: another test in this package mutates the shared owner map.
	p.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "shop-operator", "shophub.io/owner": tOwner, "shophub.io/shop": tShop,
	}
	p.ShopDomain, p.IngressClass = "shops.localhost", tTraefik
	p.IngressNamespace, p.ShopHubNamespace = tTraefik, "shophub"
	p.TrustedProxyCIDRs = "10.244.0.0/16"
	return p
}

func TestURLIsPublicHTTPS(t *testing.T) {
	p := routedParams()
	if got := URL(p.Shop, p.ShopDomain); got != "https://acme.shops.localhost" {
		t.Fatalf("URL = %q", got)
	}
}

func TestIngress(t *testing.T) {
	ing := Ingress(routedParams())
	if ing.Name != tShop || ing.Namespace != tOwner || ing.Kind != "Ingress" || ing.APIVersion != "networking.k8s.io/v1" {
		t.Fatalf("meta: %+v %+v", ing.TypeMeta, ing.ObjectMeta)
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != tTraefik {
		t.Fatalf("class = %v", ing.Spec.IngressClassName)
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("no tls block expected (default certificate), got %+v", ing.Spec.TLS)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "acme.shops.localhost" {
		t.Fatalf("rules = %+v", ing.Spec.Rules)
	}
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 || paths[0].Path != "/" || *paths[0].PathType != networkingv1.PathTypePrefix {
		t.Fatalf("paths = %+v", paths)
	}
	be := paths[0].Backend.Service
	if be == nil || be.Name != tShop || be.Port.Number != HTTPPort {
		t.Fatalf("backend = %+v", be)
	}
	if ing.Labels["shophub.io/owner"] != tOwner || ing.Labels[LabelInstance] != tShop {
		t.Fatalf("labels = %v", ing.Labels)
	}
}

func TestNetworkPolicy(t *testing.T) {
	np := NetworkPolicy(routedParams())
	if np.Name != tShop || np.Namespace != tOwner || np.Kind != "NetworkPolicy" {
		t.Fatalf("meta: %+v %+v", np.TypeMeta, np.ObjectMeta)
	}
	if got := np.Spec.PodSelector.MatchLabels; got[LabelInstance] != tShop || got[LabelName] != "shop" || len(got) != 2 {
		t.Fatalf("podSelector = %v", got)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("policyTypes = %v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %+v", np.Spec.Ingress)
	}
	rule := np.Spec.Ingress[0]
	nss := make([]string, 0, len(rule.From))
	for _, f := range rule.From {
		if f.PodSelector != nil || f.IPBlock != nil || f.NamespaceSelector == nil || len(f.NamespaceSelector.MatchLabels) != 1 {
			t.Fatalf("peer must be a namespace selector only: %+v", f)
		}
		nss = append(nss, f.NamespaceSelector.MatchLabels[LabelNamespaceName])
	}
	if len(nss) != 2 || nss[0] != tTraefik || nss[1] != "shophub" {
		t.Fatalf("namespaces = %v", nss)
	}
	if len(rule.Ports) != 1 || *rule.Ports[0].Protocol != corev1.ProtocolTCP || rule.Ports[0].Port.IntValue() != HTTPPort {
		t.Fatalf("ports = %+v", rule.Ports)
	}
}

func TestDeploymentTrustedProxyEnv(t *testing.T) {
	d := Deployment(routedParams())
	if got := envMap(d.Spec.Template.Spec.Containers[0])["TRUSTED_PROXY_CIDRS"].Value; got != "10.244.0.0/16" {
		t.Fatalf("TRUSTED_PROXY_CIDRS = %q", got)
	}
}
