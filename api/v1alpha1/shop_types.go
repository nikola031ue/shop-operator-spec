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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Availability selects how many copies of the shop application run.
// +kubebuilder:validation:Enum=standard;high
type Availability string

const (
	// AvailabilityStandard runs 2 replicas.
	AvailabilityStandard Availability = "standard"
	// AvailabilityHigh runs 3 replicas.
	AvailabilityHigh Availability = "high"
)

// Replicas returns the number of application copies for the availability level.
func (a Availability) Replicas() int32 {
	if a == AvailabilityHigh {
		return 3
	}
	return 2
}

// DatabaseType selects the database provided for a Shop.
// +kubebuilder:validation:Enum=standard;light
type DatabaseType string

const (
	// DatabaseStandard is PostgreSQL managed by CloudNativePG.
	DatabaseStandard DatabaseType = "standard"
	// DatabaseLight is Redis managed by the OT redis-operator.
	DatabaseLight DatabaseType = "light"
)

// LocalObjectReference points at an object of a known kind in the same namespace.
type LocalObjectReference struct {
	// name of the referenced object in the same namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ShopSpec defines the desired state of Shop (data-model.md "Shop").
type ShopSpec struct {
	// shopName is the human-readable shop name; immutable.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="shopName is immutable"
	ShopName string `json:"shopName"`

	// availability is standard (2 replicas) or high (3 replicas).
	// +required
	Availability Availability `json:"availability"`

	// database is standard (PostgreSQL) or light (Redis); immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="database is immutable"
	Database DatabaseType `json:"database"`

	// walletRef names the Wallet that receives this shop's payments.
	// +required
	WalletRef LocalObjectReference `json:"walletRef"`

	// discordChannelRef names the DiscordChannel this shop's alerts go to.
	// +required
	DiscordChannelRef LocalObjectReference `json:"discordChannelRef"`
}

// ShopStatus defines the observed state of Shop.
type ShopStatus struct {
	// observedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions; type Ready carries the reason codes from contracts/crds.md.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// replicas is the desired number of application copies.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// readyReplicas is the number of available application copies.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// url is the in-cluster address of the shop.
	// +optional
	URL string `json:"url,omitempty"`

	// walletAddress is the payment address currently given to the application.
	// +optional
	WalletAddress string `json:"walletAddress,omitempty"`

	// webhookSecretName is the Secret holding the referenced channel's webhook.
	// +optional
	WebhookSecretName string `json:"webhookSecretName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=shop
// +kubebuilder:printcolumn:name="Availability",type=string,JSONPath=`.spec.availability`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.database`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="metadata.name must be at most 40 characters"

// Shop is a user's shop: the application, its database and its wallet/alerting references.
type Shop struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Shop
	// +required
	Spec ShopSpec `json:"spec"`

	// status defines the observed state of Shop
	// +optional
	Status ShopStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ShopList contains a list of Shop
type ShopList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Shop `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Shop{}, &ShopList{})
		return nil
	})
}
