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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionReady is the single condition type every shophub.io kind reports (FR-017).
const ConditionReady = "Ready"

// Ready reason codes — contracts/crds.md "Status conditions".
const (
	ReasonReady                     = "Ready"
	ReasonReconcileError            = "ReconcileError"
	ReasonWalletNotReady            = "WalletNotReady"
	ReasonDiscordChannelNotReady    = "DiscordChannelNotReady"
	ReasonDatabaseOperatorMissing   = "DatabaseOperatorMissing"
	ReasonDatabaseProvisioning      = "DatabaseProvisioning"
	ReasonProgressing               = "Progressing"
	ReasonInvalidChecksum           = "InvalidChecksum"
	ReasonKeyLost                   = "KeyLost"
	ReasonInUse                     = "InUse"
	ReasonDiscordCredentialsMissing = "DiscordCredentialsMissing"
	ReasonDiscordUnavailable        = "DiscordUnavailable"
	ReasonDiscordForbidden          = "DiscordForbidden"
	// ReasonRedisCredentialsLost: a light Shop's Redis password Secret was deleted while its
	// Redis exists; it is not regenerated (spec 003 contracts/operator-0.2.0.md).
	ReasonRedisCredentialsLost = "RedisCredentialsLost"
)

// Labels (data-model.md, FR-019; research R14).
const (
	// LabelOwner identifies the owning ShopHub user; copied from the parent resource.
	LabelOwner = "shophub.io/owner"
	// LabelShop identifies the owning shop; copied from the parent resource.
	LabelShop = "shophub.io/shop"
	// LabelManagedBy marks everything the operator creates; the Secret cache filters on it.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value used by this operator.
	ManagedByValue = "shop-operator"
)

// setReady sets the Ready condition and observedGeneration on a status. It returns true if
// the condition changed. message must never contain secret values (SC-007).
func setReady(conds *[]metav1.Condition, observedGeneration *int64, generation int64,
	ready bool, reason, message string) bool {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	*observedGeneration = generation
	return meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// childLabels returns the labels every derived resource carries: managed-by plus the owner
// labels copied from the parent (FR-019). The parent's map is never modified.
func childLabels(parent map[string]string) map[string]string {
	out := map[string]string{LabelManagedBy: ManagedByValue}
	for _, k := range []string{LabelOwner, LabelShop} {
		if v, ok := parent[k]; ok {
			out[k] = v
		}
	}
	return out
}
