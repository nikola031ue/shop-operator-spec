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
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	shophubv1alpha1 "github.com/nikola031ue/shop-operator/api/v1alpha1"
	"github.com/nikola031ue/shop-operator/internal/discord"
)

const (
	// FinalizerDiscordCleanup removes the Discord channel before the resource goes away (FR-014).
	FinalizerDiscordCleanup = "shophub.io/discord-cleanup"
	// WebhookSecretKey is the data key holding the webhook URL.
	WebhookSecretKey = "url"
	// DiscordTokenKey is the data key of the bot token Secret.
	DiscordTokenKey = "token"
	// topicUIDPrefix ties a Discord channel to its resource for idempotent adoption (research R9).
	topicUIDPrefix = "shophub-uid:"
	// forbiddenRetry is the slow retry after a 401/403.
	forbiddenRetry = 5 * time.Minute
	// minDiscordRetry is the shortest requeue after a retryable Discord error.
	minDiscordRetry = time.Second
)

// WebhookSecretName is the Secret holding a DiscordChannel's webhook URL.
func WebhookSecretName(d *shophubv1alpha1.DiscordChannel) string { return d.Name + "-discord-webhook" }

// DiscordChannelReconciler reconciles a DiscordChannel object (research R9, R13;
// data-model.md "DiscordChannel").
type DiscordChannelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the unlabelled Discord token Secret, which the filtered cache cannot see
	// (research R14), and confirms cache misses on the webhook Secret.
	APIReader client.Reader
	// Config carries the Discord guild ID and token Secret location.
	Config Config
	// NewClient builds a Discord client for a token; tests inject a fake.
	NewClient func(token string) discord.Client
	// CredentialsRetry is how often missing credentials are re-checked; the token Secret is
	// hand-created and not watched. Defaults to one minute.
	CredentialsRetry time.Duration
}

// The operator never creates or deletes user resources (Principle V).
// +kubebuilder:rbac:groups=shophub.io,resources=discordchannels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=discordchannels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shophub.io,resources=discordchannels/finalizers,verbs=update
// +kubebuilder:rbac:groups=shophub.io,resources=shops,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile creates or adopts the Discord channel and its webhook, stores the webhook in an
// owned Secret, and removes the channel on deletion. The token and webhook URL never appear in
// status, events or logs (SC-007).
func (r *DiscordChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var d shophubv1alpha1.DiscordChannel
	if err := r.Get(ctx, req.NamespacedName, &d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := d.DeepCopy()

	usedBy, err := shopsReferencing(ctx, r.Client, d.Namespace, IndexShopDiscordChannelRef, d.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	d.Status.UsedBy = usedBy

	if !d.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &d, orig)
	}
	if controllerutil.AddFinalizer(&d, FinalizerDiscordCleanup) {
		if err := r.Update(ctx, &d); err != nil {
			return ctrl.Result{}, err
		}
		orig = d.DeepCopy()
		d.Status.UsedBy = usedBy
	}

	res, err := r.reconcileChannel(ctx, &d)
	if perr := r.patchStatus(ctx, &d, orig); perr != nil {
		return ctrl.Result{}, errors.Join(err, perr)
	}
	return res, err
}

func (r *DiscordChannelReconciler) reconcileDelete(ctx context.Context, d, orig *shophubv1alpha1.DiscordChannel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(d, FinalizerDiscordCleanup) {
		return ctrl.Result{}, nil
	}
	if len(d.Status.UsedBy) > 0 {
		r.setReady(d, false, ReasonInUse,
			fmt.Sprintf("deletion waits until no Shop references this DiscordChannel (used by %v)", d.Status.UsedBy))
		return ctrl.Result{}, r.patchStatus(ctx, d, orig)
	}
	if d.Status.ChannelID != "" {
		dc, res, ok := r.discordClient(ctx, d)
		if !ok {
			return res, r.patchStatus(ctx, d, orig)
		}
		if err := dc.DeleteChannel(ctx, d.Status.ChannelID); err != nil {
			res, err := r.discordError(d, err)
			if perr := r.patchStatus(ctx, d, orig); perr != nil {
				return ctrl.Result{}, errors.Join(err, perr)
			}
			return res, err
		}
	}
	controllerutil.RemoveFinalizer(d, FinalizerDiscordCleanup)
	return ctrl.Result{}, r.Update(ctx, d) // the owned webhook Secret is garbage-collected
}

func (r *DiscordChannelReconciler) reconcileChannel(ctx context.Context, d *shophubv1alpha1.DiscordChannel) (ctrl.Result, error) {
	dc, res, ok := r.discordClient(ctx, d)
	if !ok {
		return res, nil
	}

	// 1. Channel: trust status.channelID; otherwise adopt by topic UID, otherwise create.
	if d.Status.ChannelID == "" {
		marker := topicUIDPrefix + string(d.UID)
		chs, err := dc.ListChannels(ctx, r.Config.DiscordGuildID)
		if err != nil {
			return r.discordError(d, err)
		}
		for _, ch := range chs {
			if strings.Contains(ch.Topic, marker) {
				d.Status.ChannelID = ch.ID
				break
			}
		}
		if d.Status.ChannelID == "" {
			ch, err := dc.CreateChannel(ctx, r.Config.DiscordGuildID, d.Spec.ChannelName,
				"Managed by shop-operator. "+marker)
			if err != nil {
				return r.discordError(d, err)
			}
			d.Status.ChannelID = ch.ID
		}
	}

	// 2. Webhook Secret: create a webhook only if the Secret is really missing.
	key := types.NamespacedName{Namespace: d.Namespace, Name: WebhookSecretName(d)}
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, &sec) // the cache may lag behind a fresh Secret
	}
	switch {
	case apierrors.IsNotFound(err):
		url, werr := dc.CreateWebhook(ctx, d.Status.ChannelID, "shop-operator")
		if werr != nil {
			return r.discordError(d, werr)
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: childLabels(d.Labels)},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{WebhookSecretKey: []byte(url)},
		}
		if err := controllerutil.SetControllerReference(d, &sec, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	case err != nil:
		return ctrl.Result{}, err
	}

	d.Status.WebhookSecretName = key.Name
	r.setReady(d, true, ReasonReady, "channel "+d.Spec.ChannelName+" ready")
	return ctrl.Result{}, nil
}

// discordClient builds a client from the configured guild and token Secret. If credentials
// are missing it sets DiscordCredentialsMissing and returns ok=false with a retry.
func (r *DiscordChannelReconciler) discordClient(ctx context.Context, d *shophubv1alpha1.DiscordChannel) (discord.Client, ctrl.Result, bool) {
	retry := ctrl.Result{RequeueAfter: r.credentialsRetry()}
	ref := r.Config.DiscordTokenSecret
	if r.Config.DiscordGuildID == "" {
		r.setReady(d, false, ReasonDiscordCredentialsMissing,
			"no Discord server configured (chart value discord.guildID / flag --discord-guild-id)")
		return nil, retry, false
	}
	var sec corev1.Secret
	if err := r.APIReader.Get(ctx, ref, &sec); err != nil {
		r.setReady(d, false, ReasonDiscordCredentialsMissing,
			fmt.Sprintf("bot token Secret %s/%s not readable: %s", ref.Namespace, ref.Name, apierrors.ReasonForError(err)))
		return nil, retry, false
	}
	token := strings.TrimSpace(string(sec.Data[DiscordTokenKey]))
	if token == "" {
		r.setReady(d, false, ReasonDiscordCredentialsMissing,
			fmt.Sprintf("bot token Secret %s/%s has no %q key", ref.Namespace, ref.Name, DiscordTokenKey))
		return nil, retry, false
	}
	return r.NewClient(token), ctrl.Result{}, true
}

// discordError maps a Discord error to a status reason and a retry. Discord errors never
// carry the token or webhook URL (internal/discord).
func (r *DiscordChannelReconciler) discordError(d *shophubv1alpha1.DiscordChannel, err error) (ctrl.Result, error) {
	var re *discord.RetryableError
	var fe *discord.ForbiddenError
	switch {
	case errors.As(err, &re):
		r.setReady(d, false, ReasonDiscordUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: max(re.RetryAfter, minDiscordRetry)}, nil
	case errors.As(err, &fe):
		r.setReady(d, false, ReasonDiscordForbidden, err.Error())
		return ctrl.Result{RequeueAfter: forbiddenRetry}, nil
	}
	r.setReady(d, false, ReasonReconcileError, err.Error())
	return ctrl.Result{}, err
}

func (r *DiscordChannelReconciler) credentialsRetry() time.Duration {
	if r.CredentialsRetry > 0 {
		return r.CredentialsRetry
	}
	return time.Minute
}

func (r *DiscordChannelReconciler) setReady(d *shophubv1alpha1.DiscordChannel, ready bool, reason, msg string) {
	setReady(&d.Status.Conditions, &d.Status.ObservedGeneration, d.Generation, ready, reason, msg)
}

func (r *DiscordChannelReconciler) patchStatus(ctx context.Context, d, orig *shophubv1alpha1.DiscordChannel) error {
	if equality.Semantic.DeepEqual(orig.Status, d.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, d, client.MergeFrom(orig)))
}

// SetupWithManager sets up the controller with the Manager.
func (r *DiscordChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewClient == nil {
		r.NewClient = func(token string) discord.Client { return discord.NewHTTPClient(token) }
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&shophubv1alpha1.DiscordChannel{}).
		Owns(&corev1.Secret{}).
		// usedBy of the old and the new channel when a Shop changes discordChannelRef.
		Watches(&shophubv1alpha1.Shop{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				s := o.(*shophubv1alpha1.Shop)
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: s.Namespace, Name: s.Spec.DiscordChannelRef.Name}}}
			})).
		Named("discordchannel").
		Complete(r)
}
