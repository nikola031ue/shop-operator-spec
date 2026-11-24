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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
}

// The operator never creates or deletes user resources (Principle V), so no create/delete here.
// +kubebuilder:rbac:groups=shophub.io,resources=wallets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=wallets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=wallets/finalizers,verbs=update
// +kubebuilder:rbac:groups=shophub.io,resources=shops,verbs=get;list;watch
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

	// Deletion: wait while Shops reference the Wallet, then let GC remove the key Secret.
	if !w.DeletionTimestamp.IsZero() {
		if len(usedBy) > 0 {
			setReady(&w.Status.Conditions, &w.Status.ObservedGeneration, w.Generation, false, ReasonInUse,
				fmt.Sprintf("deletion waits until no Shop references this Wallet (used by %v)", usedBy))
			return ctrl.Result{}, r.patchStatus(ctx, &w, orig)
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
		return ctrl.Result{}, r.patchStatus(ctx, &w, orig)
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
	return res, r.patchStatus(ctx, &w, orig)
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
