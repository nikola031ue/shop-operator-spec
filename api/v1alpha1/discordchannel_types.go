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

// DiscordChannelSpec defines the desired state of DiscordChannel (data-model.md "DiscordChannel").
type DiscordChannelSpec struct {
	// channelName is the Discord text channel name (lowercase letters, digits, hyphens).
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]{1,100}$`
	ChannelName string `json:"channelName"`
}

// DiscordChannelStatus defines the observed state of DiscordChannel.
type DiscordChannelStatus struct {
	// observedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions; type Ready carries the reason codes from contracts/crds.md.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// channelID is the Discord channel snowflake.
	// +optional
	ChannelID string `json:"channelID,omitempty"`

	// webhookSecretName is the Secret (key "url") holding the channel webhook. The URL itself
	// never appears in status.
	// +optional
	WebhookSecretName string `json:"webhookSecretName,omitempty"`

	// usedBy lists the Shops in this namespace that reference this DiscordChannel.
	// +listType=set
	// +optional
	UsedBy []string `json:"usedBy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dch
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=`.spec.channelName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DiscordChannel is a notification channel on the project Discord server.
type DiscordChannel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DiscordChannel
	// +required
	Spec DiscordChannelSpec `json:"spec"`

	// status defines the observed state of DiscordChannel
	// +optional
	Status DiscordChannelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DiscordChannelList contains a list of DiscordChannel
type DiscordChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DiscordChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DiscordChannel{}, &DiscordChannelList{})
		return nil
	})
}
