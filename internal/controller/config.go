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
	"net/url"
	"time"

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
	"github.com/nikola031ue/shop-operator/internal/wallet"
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

	// OperatorPodSelector (k=v,...) selects the operator's own pods in OperatorNamespace(): the
	// only peers the Shop NetworkPolicy admits on the internal port (spec 005 research R4).
	OperatorPodSelector string

	// Payments configures the chain watcher and the shop app's payment settings (spec 005).
	Payments PaymentsConfig
}

// PaymentsConfig is spec 005's platform payment configuration (data-model.md "Platform settings
// and Secrets"). An empty TokenAddress turns the watcher off; Wallets report TokenNotConfigured.
type PaymentsConfig struct {
	// TokenAddress is the mUSDT contract (EIP-55 after Validate); public, from kube-state.
	TokenAddress string
	// ChainID is the chain the token lives on (Sepolia 11155111).
	ChainID int64
	// Confirmations is how many blocks must follow a transfer's block before it counts.
	Confirmations uint64
	// PollInterval is how often the watcher reads the chain head.
	PollInterval time.Duration
	// LogsBlockRange is the most blocks one log query covers (free provider plans limit it).
	LogsBlockRange uint64
	// OrderPaymentMinutes is the shop's payment window (FR-009).
	OrderPaymentMinutes int
	// ExplorerTxURL + a transaction hash is its explorer page.
	ExplorerTxURL string
	// RPCSecret locates the hand-made Secret whose key "url" is the provider endpoint. It is
	// unlabelled, so it is read by name with the uncached API reader, never listed.
	RPCSecret types.NamespacedName
	// StallAfter: a Wallet whose chain reading is further behind than this reports Stalled.
	StallAfter time.Duration
}

// DefaultPayments is the payment configuration without a token (watcher off) and the spec 005
// defaults: Sepolia, 6 confirmations, a 12 s poll, 10-block log queries, a 30-minute window.
func DefaultPayments(operatorNamespace string) PaymentsConfig {
	return PaymentsConfig{
		ChainID: 11155111, Confirmations: 6, PollInterval: 12 * time.Second, LogsBlockRange: 10,
		OrderPaymentMinutes: 30, ExplorerTxURL: "https://sepolia.etherscan.io/tx/",
		RPCSecret:  types.NamespacedName{Namespace: operatorNamespace, Name: "shop-operator-rpc"},
		StallAfter: 5 * time.Minute,
	}
}

// Enabled reports whether payments are configured (a token address is set).
func (p PaymentsConfig) Enabled() bool { return p.TokenAddress != "" }

// OperatorPodLabels returns the parsed --operator-pod-selector (validated at startup).
func (c Config) OperatorPodLabels() map[string]string {
	sel, err := labels.ConvertSelectorToLabelsMap(c.OperatorPodSelector)
	if err != nil {
		return nil
	}
	return sel
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
func (c *Config) Validate() error {
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
	if sel, err := labels.ConvertSelectorToLabelsMap(c.OperatorPodSelector); err != nil || len(sel) == 0 {
		errs = append(errs, fmt.Errorf("--operator-pod-selector must be k=v[,k=v], got %q", c.OperatorPodSelector))
	}
	errs = append(errs, c.Payments.validate()...)
	return errors.Join(errs...)
}

func (p *PaymentsConfig) validate() []error {
	var errs []error
	if p.TokenAddress != "" {
		addr, err := wallet.NormalizeAddress(p.TokenAddress)
		if err != nil {
			errs = append(errs, fmt.Errorf("--payments-token-address must be an Ethereum address, got %q", p.TokenAddress))
		}
		p.TokenAddress = addr
	}
	if p.ChainID < 1 {
		errs = append(errs, fmt.Errorf("--payments-chain-id must be positive, got %d", p.ChainID))
	}
	if p.Confirmations < 1 || p.Confirmations > 128 {
		errs = append(errs, fmt.Errorf("--payments-confirmations must be 1-128, got %d", p.Confirmations))
	}
	if p.PollInterval < time.Second || p.PollInterval > time.Minute {
		errs = append(errs, fmt.Errorf("--payments-poll-interval must be 1s-1m, got %s", p.PollInterval))
	}
	if p.LogsBlockRange < 1 || p.LogsBlockRange > 10000 {
		errs = append(errs, fmt.Errorf("--payments-logs-block-range must be 1-10000, got %d", p.LogsBlockRange))
	}
	if p.OrderPaymentMinutes < 1 || p.OrderPaymentMinutes > 24*60 {
		errs = append(errs, fmt.Errorf("--payments-order-minutes must be 1-1440, got %d", p.OrderPaymentMinutes))
	}
	if u, err := url.Parse(p.ExplorerTxURL); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("--payments-explorer-tx-url must be an https URL, got %q", p.ExplorerTxURL))
	}
	if p.RPCSecret.Name == "" || p.RPCSecret.Namespace == "" {
		errs = append(errs, errors.New("--payments-rpc-secret and the operator namespace must be set"))
	}
	if p.StallAfter < time.Minute {
		errs = append(errs, fmt.Errorf("--payments-stall-after must be at least 1m, got %s", p.StallAfter))
	}
	return errs
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
