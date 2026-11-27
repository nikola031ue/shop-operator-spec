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

// WalletMode tells whether a Wallet's address was registered or generated.
// +kubebuilder:validation:Enum=registered;generated
type WalletMode string

const (
	// WalletModeRegistered means the user supplied spec.address; no key is held.
	WalletModeRegistered WalletMode = "registered"
	// WalletModeGenerated means the operator generated the key into a Secret.
	WalletModeGenerated WalletMode = "generated"
)

// WalletSpec defines the desired state of Wallet (data-model.md "Wallet").
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.address) || (has(self.address) && self.address == oldSelf.address)",message="address is immutable"
type WalletSpec struct {
	// address is an existing Ethereum (Sepolia) address to register. Omit it to have the
	// operator generate a new account. Immutable once set.
	// +optional
	// +kubebuilder:validation:Pattern=`^0x[0-9a-fA-F]{40}$`
	Address string `json:"address,omitempty"`
}

// WalletStatus defines the observed state of Wallet.
type WalletStatus struct {
	// observedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions; type Ready carries the reason codes from contracts/crds.md.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// address is the checksummed account address (registered or generated).
	// +optional
	Address string `json:"address,omitempty"`

	// mode is registered or generated.
	// +optional
	Mode WalletMode `json:"mode,omitempty"`

	// keySecretName is the Secret holding the private key (generated mode only).
	// +optional
	KeySecretName string `json:"keySecretName,omitempty"`

	// usedBy lists the Shops in this namespace that reference this Wallet.
	// +listType=set
	// +optional
	UsedBy []string `json:"usedBy,omitempty"`

	// payments is the payment watcher's progress for this address (spec 005 research R3).
	// +optional
	Payments *WalletPayments `json:"payments,omitempty"`
}

// WalletPayments is written by the payment watcher only; the Wallet controller derives the
// PaymentsWatching condition from it (spec 005 data-model.md "Wallet status").
type WalletPayments struct {
	// scannedThroughBlock is the last confirmed block whose transfers the owning shop accepted.
	// +optional
	ScannedThroughBlock int64 `json:"scannedThroughBlock,omitempty"`

	// scannedThroughTime is that block's time.
	// +optional
	ScannedThroughTime *metav1.Time `json:"scannedThroughTime,omitempty"`

	// lastError is the reason code of the watcher's last failure for this address
	// (RPCCredentialsMissing, ProviderError, WrongChain, CursorAheadOfChain or ShopUnreachable);
	// empty after a success.
	// +optional
	LastError string `json:"lastError,omitempty"`

	// lastErrorMessage explains lastError; it never contains the provider URL or a token.
	// +optional
	LastErrorMessage string `json:"lastErrorMessage,omitempty"`

	// lastErrorTime is when lastError was last seen.
	// +optional
	LastErrorTime *metav1.Time `json:"lastErrorTime,omitempty"`
}

// Payment watcher condition and reasons (spec 005 contracts/operator.md).
const (
	// ConditionPaymentsWatching tells whether payments to this Wallet are being recognised.
	ConditionPaymentsWatching = "PaymentsWatching"

	PaymentsReasonWatching              = "Watching"
	PaymentsReasonStarting              = "Starting"
	PaymentsReasonTokenNotConfigured    = "TokenNotConfigured"
	PaymentsReasonRPCCredentialsMissing = "RPCCredentialsMissing"
	PaymentsReasonProviderError         = "ProviderError"
	PaymentsReasonShopUnreachable       = "ShopUnreachable"
	// PaymentsReasonStalled: the chain hasn't been read for this address for longer than the
	// stall threshold, although nothing reported an error (e.g. the watcher loop stopped).
	PaymentsReasonStalled = "Stalled"
	// PaymentsReasonWrongChain: the provider endpoint is on another chain than the configured
	// chain id (e.g. a mainnet URL in the RPC Secret); nothing is read until it is fixed.
	PaymentsReasonWrongChain = "WrongChain"
	// PaymentsReasonCursorAheadOfChain: this Wallet's scannedThroughBlock is above the chain's
	// head, so it was written against another chain or endpoint; this address is not read until
	// an admin clears status.payments (the watcher then starts it at the confirmed head).
	PaymentsReasonCursorAheadOfChain = "CursorAheadOfChain"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wallet
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Wallet is an Ethereum Sepolia account that receives a shop's payments.
type Wallet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Wallet
	// +optional
	Spec WalletSpec `json:"spec,omitzero"`

	// status defines the observed state of Wallet
	// +optional
	Status WalletStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WalletList contains a list of Wallet
type WalletList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Wallet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Wallet{}, &WalletList{})
		return nil
	})
}
