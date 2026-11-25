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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
)

// Config is the platform configuration passed from flags (data-model.md "Platform configuration").
type Config struct {
	// ShopImage and ShopImageTag select the placeholder Shop application image (FR-007).
	ShopImage    string
	ShopImageTag string
	// DiscordGuildID is the Discord server ID; empty means DiscordChannels report
	// DiscordCredentialsMissing.
	DiscordGuildID string
	// DiscordTokenSecret locates the bot token Secret (key "token"). It is hand-created and
	// unlabelled, so it is read with the uncached API reader (research R14, R15).
	DiscordTokenSecret types.NamespacedName

	// WatchNamespace is the only namespace whose Shops and Wallets are reconciled and cached
	// (spec 004 FR-026). DiscordChannels are also reconciled in the operator's own namespace
	// (DiscordTokenSecret.Namespace), for the platform alerts channel. Empty means every
	// namespace; only the test suite uses that, Validate refuses it.
	WatchNamespace string
	// ShopDomain is the public host suffix: a Shop is served at <shop>.<ShopDomain> (FR-021).
	ShopDomain string
	// IngressClass is spec.ingressClassName of every Shop Ingress.
	IngressClass string
	// IngressNamespace and ShopHubNamespace are the only namespaces the Shop NetworkPolicy
	// admits.
	IngressNamespace string
	ShopHubNamespace string
	// ShopHubPodSelector (k=v,...) narrows the ShopHub peer to the app pods: other pods in that
	// namespace (the database) can't reach a shop. Required.
	ShopHubPodSelector string
	// TrustedProxyHost is passed to the shop app as TRUSTED_PROXY_HOST: a DNS name (the headless
	// traefik-proxies Service) whose addresses are the only peers whose forwarded headers count.
	// Empty means no proxy is trusted.
	TrustedProxyHost string
}

// OperatorNamespace is the operator's own namespace (where the Discord token Secret lives).
func (c Config) OperatorNamespace() string { return c.DiscordTokenSecret.Namespace }

// ShopHubPodLabels returns the parsed --shophub-pod-selector (validated at startup).
func (c Config) ShopHubPodLabels() map[string]string {
	sel, err := labels.ConvertSelectorToLabelsMap(c.ShopHubPodSelector)
	if err != nil {
		return nil
	}
	return sel
}

// ShopsNamespace reports whether Shops and Wallets in ns are reconciled.
func (c Config) ShopsNamespace(ns string) bool {
	return c.WatchNamespace == "" || ns == c.WatchNamespace
}

// Validate rejects configurations that would break FR-007 (pinned image, never "latest") or
// the spec 004 routing and namespace scoping.
func (c Config) Validate() error {
	var errs []error
	for flag, ns := range map[string]string{
		"--watch-namespace": c.WatchNamespace, "--ingress-namespace": c.IngressNamespace,
		"--shophub-namespace": c.ShopHubNamespace,
	} {
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%s must be a namespace name, got %q", flag, ns))
		}
	}
	if msgs := validation.IsDNS1123Subdomain(c.ShopDomain); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("--shop-domain must be a DNS name, got %q", c.ShopDomain))
	}
	if c.IngressClass == "" {
		errs = append(errs, errors.New("--ingress-class must not be empty"))
	}
	if sel, err := labels.ConvertSelectorToLabelsMap(c.ShopHubPodSelector); err != nil || len(sel) == 0 {
		errs = append(errs, fmt.Errorf("--shophub-pod-selector must be k=v[,k=v], got %q", c.ShopHubPodSelector))
	}
	if c.TrustedProxyHost != "" {
		if msgs := validation.IsDNS1123Subdomain(c.TrustedProxyHost); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("--trusted-proxy-host must be a DNS name, got %q", c.TrustedProxyHost))
		}
	}
	if c.ShopImage == "" {
		errs = append(errs, errors.New("--shop-image must not be empty"))
	}
	if c.ShopImageTag == "" || c.ShopImageTag == "latest" {
		errs = append(errs, fmt.Errorf("--shop-image-tag must be a pinned version, got %q", c.ShopImageTag))
	}
	if c.DiscordTokenSecret.Name == "" || c.DiscordTokenSecret.Namespace == "" {
		errs = append(errs, errors.New("--discord-token-secret and the operator namespace must be set"))
	}
	return errors.Join(errs...)
}

// CacheOptions restricts the Secret cache to Secrets the operator created (research R14) and,
// with WatchNamespace set, every cached kind to that namespace (spec 004 FR-026). DiscordChannels
// and their webhook Secrets are also cached in the operator's own namespace, which holds the
// platform alerts channel; nothing else is read there except the token Secret, by name, through
// the uncached API reader. Shared by cmd/main.go and the envtest suites so all behave the same.
func (c Config) CacheOptions() cache.Options {
	managed := labels.SelectorFromSet(labels.Set{LabelManagedBy: ManagedByValue})
	if c.WatchNamespace == "" {
		return cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: managed},
		}}
	}
	both := map[string]cache.Config{c.WatchNamespace: {}, c.OperatorNamespace(): {}}
	return cache.Options{
		DefaultNamespaces: map[string]cache.Config{c.WatchNamespace: {}},
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}:                  {Label: managed, Namespaces: both},
			&shophubv1alpha1.DiscordChannel{}: {Namespaces: both},
		},
	}
}

// Database kinds created as unstructured objects (research R4, R6).
var (
	CNPGClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	RedisGVK       = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "Redis"}
)

// DatabaseKinds records which database kinds the cluster serves at startup (research R5).
type DatabaseKinds struct {
	Postgres bool
	Redis    bool
}

// DetectDatabaseKinds asks API discovery whether the CNPG Cluster and OT Redis kinds are
// served. A missing group/version is not an error: the operator still starts, and Shops that
// need the kind report DatabaseOperatorMissing.
func DetectDatabaseKinds(cfg *rest.Config) (DatabaseKinds, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return DatabaseKinds{}, err
	}
	served := func(gvk schema.GroupVersionKind) (bool, error) {
		list, err := dc.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("discovering %s: %w", gvk.GroupVersion(), err)
		}
		for _, r := range list.APIResources {
			if r.Kind == gvk.Kind {
				return true, nil
			}
		}
		return false, nil
	}
	var k DatabaseKinds
	if k.Postgres, err = served(CNPGClusterGVK); err != nil {
		return k, err
	}
	if k.Redis, err = served(RedisGVK); err != nil {
		return k, err
	}
	return k, nil
}

// Field indexes on Shop references (research R13): used for usedBy lists, deletion protection
// and re-reconciling Shops when a referenced Wallet or DiscordChannel changes.
const (
	IndexShopWalletRef         = "spec.walletRef.name"
	IndexShopDiscordChannelRef = "spec.discordChannelRef.name"
)

// SetupIndexes registers the Shop reference indexes with the manager's cache.
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &shophubv1alpha1.Shop{}, IndexShopWalletRef, func(o client.Object) []string {
		return []string{o.(*shophubv1alpha1.Shop).Spec.WalletRef.Name}
	}); err != nil {
		return err
	}
	return idx.IndexField(ctx, &shophubv1alpha1.Shop{}, IndexShopDiscordChannelRef, func(o client.Object) []string {
		return []string{o.(*shophubv1alpha1.Shop).Spec.DiscordChannelRef.Name}
	})
}
