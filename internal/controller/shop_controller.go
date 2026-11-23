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
}

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
	shop.Status.URL = resources.URL(shop)

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
		db = resources.Redis(shop, labels)
	default:
		if !r.DBKinds.Postgres {
			r.setReady(shop, false, ReasonDatabaseOperatorMissing,
				"CloudNativePG (postgresql.cnpg.io) is not installed; install the cnpg release and restart shop-operator")
			return ctrl.Result{}, nil
		}
		db = resources.CNPGCluster(shop, labels)
	}
	if err := r.apply(ctx, shop, db); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying %s %s: %w", db.GetKind(), db.GetName(), err)
	}

	// 3. Application (FR-002, FR-004, FR-007).
	image, err := resources.ImageRef(r.Config.ShopImage, r.Config.ShopImageTag)
	if err != nil {
		return ctrl.Result{}, err
	}
	p := resources.ShopParams{Shop: shop, Image: image, WalletAddress: w.Status.Address, Labels: labels}
	if err := r.apply(ctx, shop, resources.Deployment(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying Deployment: %w", err)
	}
	if err := r.apply(ctx, shop, resources.Service(p)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying Service: %w", err)
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
		return err == nil && sts.Status.ReadyReplicas >= 1, err
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
