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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/resources"
)

const (
	// FieldOwner is the server-side-apply field manager of everything the operator applies.
	FieldOwner = "shop-operator"
	// notReadyRequeue re-checks children that are not watched (e.g. the Redis StatefulSet)
	// while a Shop is not Ready (research R12).
	notReadyRequeue = 30 * time.Second
)

// ShopReconciler reconciles a Shop object (research R11, R12; data-model.md "Shop").
type ShopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Config carries the placeholder image settings (FR-007).
	Config Config
	// DBKinds records which database kinds the cluster served at startup (research R5).
	DBKinds DatabaseKinds
	// APIReader confirms a generated password Secret is really absent before creating one; the
	// cache may lag behind a fresh Secret (003 R1). Falls back to Client when nil.
	APIReader client.Reader
}

// redisRecreateRequeue re-checks soon after deleting a 0.1.0 Redis (003 R4).
const redisRecreateRequeue = 5 * time.Second

// The operator reads but never creates or deletes user resources (Principle V).
// +kubebuilder:rbac:groups=shophub.io,resources=shops,verbs=get;list;watch
// +kubebuilder:rbac:groups=shophub.io,resources=shops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=wallets;discordchannels,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redis.opstreelabs.in,resources=redis,verbs=get;list;watch;create;update;patch;delete
// Redis readiness is read from the StatefulSet the OT operator creates (research R12, R14).
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// Generated admin and Redis password Secrets (spec 003 contracts/operator-0.2.0.md).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// Per-Shop routing children (spec 004 operator 0.3.0).
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a Shop's application, Service and database toward its spec and reports
// readiness. Children carry a controller owner reference, so deleting the Shop garbage-collects
// them (FR-006); no finalizer is needed.
func (r *ShopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var shop shophubv1alpha1.Shop
	if err := r.Get(ctx, req.NamespacedName, &shop); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !shop.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !r.Config.ShopsNamespace(shop.Namespace) {
		return ctrl.Result{}, nil // the cache never delivers these; defence in depth (FR-026)
	}
	orig := shop.DeepCopy()

	res, err := r.reconcile(ctx, &shop)
	if err != nil {
		log.Error(err, "Reconciling shop failed", "shop", req.NamespacedName)
		r.setReady(&shop, false, ReasonReconcileError, err.Error())
	}
	if perr := r.patchStatus(ctx, &shop, orig); perr != nil {
		return ctrl.Result{}, errors.Join(err, perr)
	}
	return res, err
}

func (r *ShopReconciler) reconcile(ctx context.Context, shop *shophubv1alpha1.Shop) (ctrl.Result, error) {
	desired := shop.Spec.Availability.Replicas()
	shop.Status.Replicas = desired
	shop.Status.URL = resources.URL(shop, r.Config.ShopDomain)

	// 1. References: both must be Ready before the app is deployed (FR-001).
	var w shophubv1alpha1.Wallet
	if ok, msg, err := r.refReady(ctx, shop.Namespace, shop.Spec.WalletRef.Name, &w, "Wallet"); err != nil || !ok {
		if err == nil {
			r.setReady(shop, false, ReasonWalletNotReady, msg)
		}
		return ctrl.Result{}, err
	}
	if w.Status.Address == "" {
		r.setReady(shop, false, ReasonWalletNotReady, fmt.Sprintf("Wallet %s has no address yet", w.Name))
		return ctrl.Result{}, nil
	}
	shop.Status.WalletAddress = w.Status.Address

	var ch shophubv1alpha1.DiscordChannel
	if ok, msg, err := r.refReady(ctx, shop.Namespace, shop.Spec.DiscordChannelRef.Name, &ch, "DiscordChannel"); err != nil || !ok {
		if err == nil {
			r.setReady(shop, false, ReasonDiscordChannelNotReady, msg)
		}
		return ctrl.Result{}, err
	}
	shop.Status.WebhookSecretName = ch.Status.WebhookSecretName

	// 2. Database (FR-003). A kind the cluster does not serve is reported, not retried: the
	// operator must be restarted after installing the database operator (research R5).
	labels := childLabels(shop.Labels)
	var db *unstructured.Unstructured
	switch shop.Spec.Database {
	case shophubv1alpha1.DatabaseLight:
		if !r.DBKinds.Redis {
			r.setReady(shop, false, ReasonDatabaseOperatorMissing,
				"the Redis operator (redis.redis.opstreelabs.in) is not installed; install the redis-operator release and restart shop-operator")
			return ctrl.Result{}, nil
		}
		// Built in 2a, once the Redis password Secret name is known.
	default:
		if !r.DBKinds.Postgres {
			r.setReady(shop, false, ReasonDatabaseOperatorMissing,
				"CloudNativePG (postgresql.cnpg.io) is not installed; install the cnpg release and restart shop-operator")
			return ctrl.Result{}, nil
		}
		db = resources.CNPGCluster(shop, labels)
	}

	// 2a. Generated passwords (spec 003 R1, R2) and the 0.1.0 Redis upgrade (R4).
	adminSec, err := r.ensurePasswordSecret(ctx, shop, resources.AdminSecretName(shop), resources.AdminPasswordLength, labels)
	if err != nil {
		return ctrl.Result{}, err
	}
	var redisSec *corev1.Secret
	if shop.Spec.Database == shophubv1alpha1.DatabaseLight {
		res, sec, done, err := r.prepareRedis(ctx, shop, labels)
		if err != nil || done {
			return res, err
		}
		redisSec = sec
		db = resources.Redis(shop, labels, sec.Name)
	}

	if err := r.apply(ctx, shop, db); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying %s %s: %w", db.GetKind(), db.GetName(), err)
	}

	// 3. Application (FR-002, FR-004, FR-007).
	image, err := resources.ImageRef(r.Config.ShopImage, r.Config.ShopImageTag)
	if err != nil {
		return ctrl.Result{}, err
	}
	p := resources.ShopParams{
		Shop: shop, Image: image, WalletAddress: w.Status.Address, Labels: labels,
		AdminSecretUID: string(adminSec.UID),
		ShopDomain:     r.Config.ShopDomain, IngressClass: r.Config.IngressClass,
		IngressNamespace: r.Config.IngressNamespace, ShopHubNamespace: r.Config.ShopHubNamespace,
		ShopHubPodLabels: r.Config.ShopHubPodLabels(),
		TrustedProxyHost: r.Config.TrustedProxyHost,
	}
	if redisSec != nil {
		p.RedisSecretUID, p.RedisSecretName = string(redisSec.UID), redisSec.Name
	}
	// The NetworkPolicy goes first so the pods are never reachable from outside the platform.
	if err := r.apply(ctx, shop, resources.NetworkPolicy(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying NetworkPolicy: %w", err)
	}
	if err := r.apply(ctx, shop, resources.Deployment(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying Deployment: %w", err)
	}
	if err := r.apply(ctx, shop, resources.Service(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying Service: %w", err)
	}
	if err := r.apply(ctx, shop, resources.Ingress(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying Ingress: %w", err)
	}

	// 4. Readiness (research R12).
	dbReady, err := r.databaseReady(ctx, shop)
	if err != nil {
		return ctrl.Result{}, err
	}
	available, err := r.availableReplicas(ctx, shop, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	shop.Status.ReadyReplicas = available
	switch {
	case !dbReady:
		r.setReady(shop, false, ReasonDatabaseProvisioning, fmt.Sprintf("waiting for %s %s", db.GetKind(), db.GetName()))
		return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
	case available < desired:
		r.setReady(shop, false, ReasonProgressing, fmt.Sprintf("%d of %d copies available", available, desired))
		return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
	}
	r.setReady(shop, true, ReasonReady, fmt.Sprintf("%d copies available", available))
	return ctrl.Result{}, nil
}

func (r *ShopReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// getSecret reads a Secret from the cache and, on a miss, confirms with the API server.
func (r *ShopReconciler) getSecret(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	key := types.NamespacedName{Namespace: ns, Name: name}
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		err = r.reader().Get(ctx, key, &sec)
	}
	if err != nil {
		return nil, err
	}
	return &sec, nil
}

// ensurePasswordSecret returns the named password Secret, creating it once with a new random
// value if it does not exist. An existing Secret is never updated (003 contract).
func (r *ShopReconciler) ensurePasswordSecret(ctx context.Context, shop *shophubv1alpha1.Shop,
	name string, length int, labels map[string]string) (*corev1.Secret, error) {
	sec, err := r.getSecret(ctx, shop.Namespace, name)
	if err == nil {
		return sec, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	pw, err := resources.RandomPassword(length)
	if err != nil {
		return nil, err
	}
	sec = resources.PasswordSecret(shop, name, labels, pw)
	if err := controllerutil.SetControllerReference(shop, sec, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.getSecret(ctx, shop.Namespace, name)
		}
		return nil, fmt.Errorf("creating Secret %s: %w", name, err)
	}
	logf.FromContext(ctx).Info("Created generated password Secret", "secret", name) // name only (SC-007)
	return sec, nil
}

// prepareRedis upgrades a 0.1.0 Redis and returns the Redis password Secret the Redis and the
// app must use, creating it if needed. done=true means the caller must stop and return res
// (status is set).
//
// The current Secret name is the one the live Redis references. If that Secret was deleted, a
// new password is generated under the next name (operator 0.2.1, research R2): the changed
// name makes the Redis operator roll the Redis pod with the new password, keeping the PVC and
// its data, and the app pods roll because their secretKeyRef and UID annotation change. The
// Redis and its PVC are never deleted for this.
func (r *ShopReconciler) prepareRedis(ctx context.Context, shop *shophubv1alpha1.Shop,
	labels map[string]string) (res ctrl.Result, sec *corev1.Secret, done bool, err error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(RedisGVK)
	err = r.Get(ctx, types.NamespacedName{Namespace: shop.Namespace, Name: resources.RedisName(shop)}, live)
	switch {
	case apierrors.IsNotFound(err):
		live = nil
	case err != nil:
		return ctrl.Result{}, nil, true, err
	}

	name := resources.RedisAuthSecretName(shop)
	if live != nil {
		if !live.GetDeletionTimestamp().IsZero() {
			r.setReady(shop, false, ReasonDatabaseProvisioning,
				fmt.Sprintf("waiting for the old Redis %s to be removed", live.GetName()))
			return ctrl.Result{RequeueAfter: redisRecreateRequeue}, nil, true, nil
		}
		// A Redis without storage was created by operator 0.1.0. StatefulSet volume claims are
		// immutable, so it is recreated once; 0.1.0 light shops hold no persistent data (R4).
		if _, found, _ := unstructured.NestedMap(live.Object, "spec", "storage"); !found {
			logf.FromContext(ctx).Info("Recreating Redis without storage (0.1.0 upgrade)", "redis", live.GetName())
			if err := r.Delete(ctx, live, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, nil, true, fmt.Errorf("deleting 0.1.0 Redis %s: %w", live.GetName(), err)
			}
			r.setReady(shop, false, ReasonDatabaseProvisioning,
				fmt.Sprintf("recreating Redis %s with password and storage", live.GetName()))
			return ctrl.Result{RequeueAfter: redisRecreateRequeue}, nil, true, nil
		}
		if cur, _, _ := unstructured.NestedString(live.Object, "spec", "kubernetesConfig", "redisSecret", "name"); resources.RedisAuthSecretGeneration(shop, cur) > 0 {
			name = cur
		}
	}

	sec, err = r.getSecret(ctx, shop.Namespace, name)
	if err == nil {
		return ctrl.Result{}, sec, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil, true, err
	}
	if live != nil {
		// The running Redis keeps the lost password until it restarts; switch it to a new Secret.
		next := resources.NextRedisAuthSecretName(shop, name)
		logf.FromContext(ctx).Info("Redis password Secret is missing; restarting Redis with a new password Secret",
			"redis", live.GetName(), "missingSecret", name, "secret", next) // names only (SC-007)
		name = next
	}
	sec, err = r.ensurePasswordSecret(ctx, shop, name, resources.RedisPasswordLength, labels)
	if err != nil {
		return ctrl.Result{}, nil, true, err
	}
	return ctrl.Result{}, sec, false, nil
}

// refReady reads a referenced Wallet/DiscordChannel into obj and reports whether it is Ready.
func (r *ShopReconciler) refReady(ctx context.Context, ns, name string, obj client.Object, kind string) (bool, string, error) {
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("%s %s not found", kind, name), nil
		}
		return false, "", err
	}
	var conds []metav1.Condition
	switch o := obj.(type) {
	case *shophubv1alpha1.Wallet:
		conds = o.Status.Conditions
	case *shophubv1alpha1.DiscordChannel:
		conds = o.Status.Conditions
	}
	if !meta.IsStatusConditionTrue(conds, ConditionReady) {
		return false, fmt.Sprintf("%s %s is not Ready", kind, name), nil
	}
	return true, "", nil
}

// apply server-side applies a child with a controller owner reference to the Shop.
func (r *ShopReconciler) apply(ctx context.Context, shop *shophubv1alpha1.Shop, obj client.Object) error {
	if err := controllerutil.SetControllerReference(shop, obj, r.Scheme); err != nil {
		return err
	}
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	return r.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// toUnstructured converts a typed object for server-side apply, dropping fields the operator
// must not claim (status, null creation timestamps).
func toUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u, nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	return u, nil
}

func (r *ShopReconciler) databaseReady(ctx context.Context, shop *shophubv1alpha1.Shop) (bool, error) {
	if shop.Spec.Database == shophubv1alpha1.DatabaseLight {
		var sts appsv1.StatefulSet
		err := r.Get(ctx, types.NamespacedName{Namespace: shop.Namespace, Name: resources.RedisName(shop)}, &sts)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// Not ready while the Redis operator's change has not been rolled out (e.g. a restart
		// with a new password Secret, operator 0.2.1).
		rolledOut := sts.Status.ObservedGeneration >= sts.Generation &&
			(sts.Status.UpdateRevision == "" || sts.Status.CurrentRevision == sts.Status.UpdateRevision)
		return rolledOut && sts.Status.ReadyReplicas >= 1, nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CNPGClusterGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: shop.Namespace, Name: resources.CNPGClusterName(shop)}, u)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" && m["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

func (r *ShopReconciler) availableReplicas(ctx context.Context, shop *shophubv1alpha1.Shop, desired int32) (int32, error) {
	var d appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: shop.Namespace, Name: resources.DeploymentName(shop)}, &d)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < desired {
		return min(d.Status.AvailableReplicas, desired-1), nil // rollout still in progress
	}
	return d.Status.AvailableReplicas, nil
}

func (r *ShopReconciler) setReady(shop *shophubv1alpha1.Shop, ready bool, reason, msg string) {
	setReady(&shop.Status.Conditions, &shop.Status.ObservedGeneration, shop.Generation, ready, reason, msg)
}

func (r *ShopReconciler) patchStatus(ctx context.Context, shop, orig *shophubv1alpha1.Shop) error {
	if equality.Semantic.DeepEqual(orig.Status, shop.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, shop, client.MergeFrom(orig)))
}

// shopsReferencingFn maps a Wallet/DiscordChannel event to the Shops that reference it.
func (r *ShopReconciler) shopsReferencingFn(field string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		if !r.Config.ShopsNamespace(o.GetNamespace()) {
			return nil // e.g. the platform alerts channel in the operator namespace
		}
		var shops shophubv1alpha1.ShopList
		if err := r.List(ctx, &shops, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{field: o.GetName()}); err != nil {
			logf.FromContext(ctx).Error(err, "Listing shops for reference", "field", field)
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(shops.Items))
		for _, s := range shops.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
		}
		return reqs
	}
}

// SetupWithManager sets up the controller with the Manager. Database kinds are only watched
// if the cluster serves them (research R5).
func (r *ShopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&shophubv1alpha1.Shop{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.Secret{}). // generated password Secrets (003); the cache is label-filtered
		Watches(&shophubv1alpha1.Wallet{}, handler.EnqueueRequestsFromMapFunc(r.shopsReferencingFn(IndexShopWalletRef))).
		Watches(&shophubv1alpha1.DiscordChannel{}, handler.EnqueueRequestsFromMapFunc(r.shopsReferencingFn(IndexShopDiscordChannelRef)))
	if r.DBKinds.Postgres {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(CNPGClusterGVK)
		b = b.Owns(u)
	}
	if r.DBKinds.Redis {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(RedisGVK)
		b = b.Owns(u)
	}
	return b.Named("shop").Complete(r)
}
