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

// SecretKeyReference points to one key in a Secret in the same namespace as
// the VirtualKey. The value of that key is the virtual key that LiteLLM accepts.
type SecretKeyReference struct {
	// name of the Secret, in the namespace of the VirtualKey.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`

	// key in the Secret's data that holds the virtual key value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Key string `json:"key"`
}

// VirtualKeySpec defines the desired state of a LiteLLM virtual key.
type VirtualKeySpec struct {
	// alias is the LiteLLM key_alias. The operator finds the key in LiteLLM by
	// this alias. It cannot change after creation, because a new alias would
	// leave the old key behind in LiteLLM. Reserve nine characters for the
	// "-rotating" suffix within LiteLLM's optional 255-character alias limit.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=246
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="alias is immutable"
	// +required
	Alias string `json:"alias"`

	// secretRef points to the Secret key that holds the virtual key value.
	// +required
	SecretRef SecretKeyReference `json:"secretRef"`

	// models the key may call. LiteLLM treats an empty list as "all models",
	// so at least one entry is required.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +listType=set
	// +required
	Models []string `json:"models"`

	// userID is the LiteLLM user_id that owns the key.
	// +optional
	UserID string `json:"userID,omitempty"`

	// metadata is the LiteLLM key metadata, as flat string pairs.
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Condition types and reasons for VirtualKey status.
const (
	// ConditionReady is True when the LiteLLM key matches the spec and the Secret.
	ConditionReady = "Ready"
)

// VirtualKeyStatus defines the observed state of VirtualKey.
type VirtualKeyStatus struct {
	// observedGeneration is the .metadata.generation that the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the state of the key. The main type is Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vk
// +kubebuilder:printcolumn:name="Alias",type=string,JSONPath=`.spec.alias`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualKey is a LiteLLM virtual key whose value comes from a Kubernetes Secret.
type VirtualKey struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VirtualKey
	// +required
	Spec VirtualKeySpec `json:"spec"`

	// status defines the observed state of VirtualKey
	// +optional
	Status VirtualKeyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VirtualKeyList contains a list of VirtualKey
type VirtualKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VirtualKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VirtualKey{}, &VirtualKeyList{})
		return nil
	})
}
