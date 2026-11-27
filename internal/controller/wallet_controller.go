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
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/shopapi"
	"github.com/nikola031ue/shop-operator/internal/wallet"
)

const (
	// FinalizerInUse blocks deletion of a Wallet/DiscordChannel while Shops reference it (FR-010).
	FinalizerInUse = "shophub.io/in-use-protection"
	// WalletKeySecretKey is the data key of the generated private key.
	WalletKeySecretKey = "privateKey"
)

// WalletKeySecretName is the Secret holding a generated Wallet's private key.
func WalletKeySecretName(w *shophubv1alpha1.Wallet) string { return w.Name + "-wallet-key" }

// WalletReconciler reconciles a Wallet object (research R10, R13; data-model.md "Wallet").
type WalletReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Config limits reconciliation to the shops namespace (spec 004 FR-026).
	Config Config
	// APIReader confirms a key Secret is really gone before reporting KeyLost; the cache may
	// lag right after the Secret was created.
	APIReader client.Reader
	// Shops asks a shop how many orders still wait on an address (spec 005 D-1).
	Shops shopapi.Client
	// Now is the clock for the stall check; nil means time.Now.
	Now func() time.Time
	// Recheck is how often PaymentsWatching and a held deletion are re-evaluated; zero means
	// DefaultPaymentsRecheck.
	Recheck time.Duration
}

const (
	// ReasonWaitingForOrders: a Wallet being deleted is kept until its shop runs only the new
	// address and no order waits on it (spec 005 D-1, research R9).
	ReasonWaitingForOrders = "WaitingForOrders"
	// AnnotationDeleteRequested is written by ShopHub right before it deletes a Shop (D-4).
	AnnotationDeleteRequested = "shophub.io/delete-requested"
	// DefaultPaymentsRecheck is how often a Wallet's PaymentsWatching condition and a held
	// deletion are re-evaluated.
	DefaultPaymentsRecheck = time.Minute
)

// The operator never creates or deletes user resources (Principle V), so no create/delete here.
// +kubebuilder:rbac:groups=shophub.io,resources=wallets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=wallets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=wallets/finalizers,verbs=update
// +kubebuilder:rbac:groups=shophub.io,resources=shops,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile registers or generates the Wallet's account and keeps usedBy and deletion
// protection current. Log lines and status never contain the private key (SC-007).
func (r *WalletReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var w shophubv1alpha1.Wallet
	if err := r.Get(ctx, req.NamespacedName, &w); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.Config.ShopsNamespace(w.Namespace) {
		return ctrl.Result{}, nil // the cache never delivers these; defence in depth (FR-026)
	}
	orig := w.DeepCopy()

	usedBy, err := shopsReferencing(ctx, r.Client, w.Namespace, IndexShopWalletRef, w.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	w.Status.UsedBy = usedBy

	r.setPaymentsCondition(&w)

	// Deletion: wait while Shops reference the Wallet, then (spec 005 D-1) while its shop still
	// runs copies with this address or has orders waiting on it; then let GC remove the key Secret.
	if !w.DeletionTimestamp.IsZero() {
		if len(usedBy) > 0 {
			setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonInUse,
				fmt.Sprintf("deletion waits until no Shop references this Wallet (used by %v)", usedBy))
			return ctrl.Result{}, r.patchStatus(ctx, &w, orig)
		}
		if ok, why := r.releasable(ctx, &w); !ok {
			setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonWaitingForOrders, why)
			return ctrl.Result{RequeueAfter: r.recheck()}, r.patchStatus(ctx, &w, orig)
		}
		if controllerutil.RemoveFinalizer(&w, FinalizerInUse) {
			return ctrl.Result{}, r.Update(ctx, &w)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&w, FinalizerInUse) {
		if err := r.Update(ctx, &w); err != nil {
			return ctrl.Result{}, err
		}
		orig = w.DeepCopy()
		w.Status.UsedBy = usedBy
	}

	if w.Spec.Address != "" {
		r.reconcileRegistered(&w)
		return r.paymentsRequeue(), r.patchStatus(ctx, &w, orig)
	}

	res, err := r.reconcileGenerated(ctx, &w)
	if err != nil {
		log.Error(err, "Reconciling generated wallet failed", "wallet", req.NamespacedName)
		setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonReconcileError, err.Error())
		if perr := r.patchStatus(ctx, &w, orig); perr != nil {
			return ctrl.Result{}, errors.Join(err, perr)
		}
		return ctrl.Result{}, err
	}
	if res.IsZero() {
		res = r.paymentsRequeue()
	}
	return res, r.patchStatus(ctx, &w, orig)
}

// paymentsRequeue re-evaluates PaymentsWatching every minute while payments are on, so a watcher
// that stopped shows up as Stalled even though nothing writes to the Wallet any more.
func (r *WalletReconciler) paymentsRequeue() ctrl.Result {
	if r.Config.Payments.Enabled() {
		return ctrl.Result{RequeueAfter: r.recheck()}
	}
	return ctrl.Result{}
}

func (r *WalletReconciler) recheck() time.Duration {
	if r.Recheck > 0 {
		return r.Recheck
	}
	return DefaultPaymentsRecheck
}

func (r *WalletReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// setPaymentsCondition derives PaymentsWatching from the watcher's facts in status.payments. The
// watcher writes only those facts; this controller is the condition's only writer.
func (r *WalletReconciler) setPaymentsCondition(w *shophubv1alpha1.Wallet) {
	ok, reason, message := PaymentsWatching(r.Config.Payments, w.Status.Payments, r.now())
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
		Type: shophubv1alpha1.ConditionPaymentsWatching, Status: status, Reason: reason, Message: message,
		ObservedGeneration: w.Generation,
	})
}

// PaymentsWatching is spec 005's watcher condition for one Wallet: off without a token address;
// the watcher's own error when it reported one; Starting before the first pass; Stalled when the
// chain hasn't been read for this address for longer than StallAfter (orders can't expire then,
// FR-011); otherwise Watching.
func PaymentsWatching(cfg PaymentsConfig, p *shophubv1alpha1.WalletPayments, now time.Time) (bool, string, string) {
	switch {
	case !cfg.Enabled():
		return false, shophubv1alpha1.PaymentsReasonTokenNotConfigured, "no mUSDT token address is configured; payments are off"
	case p != nil && p.LastError != "":
		return false, p.LastError, p.LastErrorMessage
	case p == nil || p.ScannedThroughTime == nil:
		return false, shophubv1alpha1.PaymentsReasonStarting, "the payment watcher has not read the chain for this address yet"
	}
	lag := now.Sub(p.ScannedThroughTime.Time).Round(time.Second)
	if lag > cfg.StallAfter {
		return false, shophubv1alpha1.PaymentsReasonStalled, fmt.Sprintf(
			"the chain was last read through block %d (%s ago, more than %s); unpaid orders don't expire until it catches up",
			p.ScannedThroughBlock, lag, cfg.StallAfter)
	}
	return true, shophubv1alpha1.PaymentsReasonWatching, fmt.Sprintf("read through block %d", p.ScannedThroughBlock)
}

// releasable reports whether a Wallet no Shop references may go (spec 005 D-1, research R9): its
// shop is gone or being deleted, payments are off, or the shop runs only copies without this
// address (rollout done, D-5) and says no order waits on it. Otherwise it says why it waits.
func (r *WalletReconciler) releasable(ctx context.Context, w *shophubv1alpha1.Wallet) (bool, string) {
	name := w.Labels[LabelShop]
	if name == "" || !r.Config.Payments.Enabled() {
		return true, ""
	}
	var shop shophubv1alpha1.Shop
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: name}, &shop); err != nil {
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		return false, fmt.Sprintf("reading Shop %s failed: %v", name, err)
	}
	if !shop.DeletionTimestamp.IsZero() {
		return true, "" // the whole shop goes, with its orders
	}
	var d appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: shop.Name}, &d); err != nil {
		return false, fmt.Sprintf("reading the shop's Deployment failed: %v", err)
	}
	if RolloutInProgress(&d, shop.Spec.Availability.Replicas()) {
		return false, "the shop is still rolling out; a copy may take orders to this address"
	}
	if r.Shops == nil {
		return false, "no shop client configured"
	}
	target, err := shopapi.ShopFor(ctx, r.secretData, w.Namespace, shop.Name)
	if err != nil {
		return false, "the shop's internal token is not readable: " + err.Error()
	}
	n, err := r.Shops.Waiting(ctx, target, w.Status.Address)
	if err != nil {
		return false, "asking the shop for waiting orders failed: " + err.Error()
	}
	if n > 0 {
		return false, fmt.Sprintf("%d order(s) to %s still wait for payment; kept until they are paid or expired", n, w.Status.Address)
	}
	return true, ""
}

func (r *WalletReconciler) secretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return sec.Data, nil
}

func (r *WalletReconciler) reconcileRegistered(w *shophubv1alpha1.Wallet) {
	w.Status.Mode = shophubv1alpha1.WalletModeRegistered
	w.Status.KeySecretName = ""
	addr, err := wallet.NormalizeAddress(w.Spec.Address)
	if err != nil {
		w.Status.Address = ""
		setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonInvalidChecksum,
			"spec.address fails the EIP-55 checksum; use the checksummed or the all-lowercase form")
		return
	}
	w.Status.Address = addr
	setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, true, ReasonReady,
		"registered address "+addr)
}

func (r *WalletReconciler) reconcileGenerated(ctx context.Context, w *shophubv1alpha1.Wallet) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: w.Namespace, Name: WalletKeySecretName(w)}
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		// The cache may lag behind a Secret created moments ago: ask the API server.
		err = r.APIReader.Get(ctx, key, &sec)
	}
	switch {
	case err == nil:
		addr, derr := wallet.AddressFromPrivateKeyHex(string(sec.Data[WalletKeySecretKey]))
		if derr != nil {
			return ctrl.Result{}, fmt.Errorf("key Secret %s holds no valid private key", key.Name)
		}
		w.Status.Mode = shophubv1alpha1.WalletModeGenerated
		w.Status.Address = addr
		w.Status.KeySecretName = key.Name
		setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, true, ReasonReady,
			"generated address "+addr)
		return ctrl.Result{}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// The Secret is really gone.
	if w.Status.Mode == shophubv1alpha1.WalletModeGenerated && w.Status.Address != "" {
		// Never generate a replacement: it would silently change the payment address.
		setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonKeyLost,
			fmt.Sprintf("key Secret %s was deleted; the private key for %s is lost. "+
				"Create a new Wallet, move Shops to it, then delete this one", key.Name, w.Status.Address))
		return ctrl.Result{}, nil
	}

	priv, addr, err := wallet.GenerateKey()
	if err != nil {
		return ctrl.Result{}, err
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: childLabels(w.Labels)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{WalletKeySecretKey: []byte(priv)},
	}
	if err := controllerutil.SetControllerReference(w, &sec, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, &sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created by an earlier attempt we can't see yet: read it next time, never overwrite.
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	w.Status.Mode = shophubv1alpha1.WalletModeGenerated
	w.Status.Address = addr
	w.Status.KeySecretName = key.Name
	setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, true, ReasonReady,
		"generated address "+addr)
	return ctrl.Result{}, nil
}

func (r *WalletReconciler) patchStatus(ctx context.Context, w, orig *shophubv1alpha1.Wallet) error {
	if equality.Semantic.DeepEqual(orig.Status, w.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, w, client.MergeFrom(orig)))
}

// shopsReferencing returns the sorted names of Shops in ns whose indexed field equals name.
func shopsReferencing(ctx context.Context, c client.Client, ns, field, name string) ([]string, error) {
	var shops shophubv1alpha1.ShopList
	if err := c.List(ctx, &shops, client.InNamespace(ns), client.MatchingFields{field: name}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(shops.Items))
	for _, s := range shops.Items {
		if s.DeletionTimestamp.IsZero() {
			names = append(names, s.Name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, nil
	}
	return names, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WalletReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&shophubv1alpha1.Wallet{}).
		Owns(&corev1.Secret{}).
		// A Shop changing its walletRef updates usedBy of both the old and the new Wallet:
		// map functions run for the old and the new object on updates.
		Watches(&shophubv1alpha1.Shop{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				s := o.(*shophubv1alpha1.Shop)
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: s.Namespace, Name: s.Spec.WalletRef.Name}}}
			})).
		Named("wallet").
		Complete(r)
}
