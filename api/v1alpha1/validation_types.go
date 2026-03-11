/*
Copyright 2025.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ValidationOwnerKey = ".validation.metadata.controller"

	// Phase constants
	ValidationPhaseEmpty     = ""
	ValidationPhasePending   = "Pending"
	ValidationPhaseRunning   = "Running"
	ValidationPhaseSucceeded = "Succeeded"
	ValidationPhaseFailed    = "Failed"

	// Condition types
	ValidationConditionPodCreated    = "PodCreated"
	ValidationConditionTestCompleted = "TestCompleted"
	ValidationConditionTestPassed    = "TestPassed"
)

// ValidationSpec defines the desired state of Validation.
type ValidationSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://dev\.azure\.com/.+`
	PrUrl string `json:"prUrl"`
	// +kubebuilder:validation:Required
	Container corev1.Container `json:"container"`
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxRetries int32 `json:"maxRetries,omitempty"`
}

// ValidationStatus defines the observed state of Validation.
type ValidationStatus struct {
	Phase      string             `json:"phase"`
	RetryCount int32              `json:"retryCount"`
	PodName    string             `json:"podName"`
	Conditions []metav1.Condition `json:"conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Retries",type="integer",JSONPath=`.status.retryCount`
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=`.status.podName`

// Validation is the Schema for the validations API.
type Validation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ValidationSpec   `json:"spec,omitempty"`
	Status ValidationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ValidationList contains a list of Validation.
type ValidationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Validation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Validation{}, &ValidationList{})
}
