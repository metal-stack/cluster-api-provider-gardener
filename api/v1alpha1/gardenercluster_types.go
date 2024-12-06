/*
Copyright 2024.

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

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capierrors "sigs.k8s.io/cluster-api/errors"
)

const (
	// ClusterFinalizer allows to clean up resources associated with before removing it from the apiserver.
	ClusterFinalizer = "gardener.infrastructure.cluster.x-k8s.io/cluster"

	TagInfraClusterID = "gardener.infrastructure.cluster.x-k8s.io/cluster-id"

	GardenerProjectEnsured clusterv1.ConditionType = "GardenerProjectEnsured"
	GardenerShootReady     clusterv1.ConditionType = "GardenerShootReady"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// GardenerClusterSpec defines the desired state of GardenerCluster.
type GardenerClusterSpec struct {
	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +optional
	ControlPlaneEndpoint APIEndpoint `json:"controlPlaneEndpoint"`

	// ShootSpec is the spec for the Gardener shoot cluster.
	ShootSpec runtime.RawExtension `json:"shootSpec"`

	// ProviderSecretRef is a reference to a secret that contains provider credentials for the shoot.
	ProviderSecretRef SecretRef `json:"providerSecretRef"`
}

type SecretRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// APIEndpoint represents a reachable Kubernetes API endpoint.
type APIEndpoint struct {
	// Host is the hostname on which the API server is serving.
	Host string `json:"host"`

	// Port is the port on which the API server is serving.
	Port int `json:"port"`
}

// GardenerClusterStatus defines the observed state of GardenerCluster.
type GardenerClusterStatus struct {
	// Ready denotes that the cluster is ready.
	Ready bool `json:"ready"`

	// Initialized denotes whether or not the control plane has the
	// uploaded kubernetes config-map.
	Initialized bool `json:"initialized"`

	// ExternalManagedControlPlane indicates to cluster-api that the control plane
	// is managed by an external service such as AKS, EKS, GKE, etc.
	// +kubebuilder:default=true
	ExternalManagedControlPlane *bool `json:"externalManagedControlPlane,omitempty"`

	// FailureReason indicates that there is a fatal problem reconciling the
	// state, and will be set to a token value suitable for
	// programmatic interpretation.
	// +optional
	FailureReason *capierrors.ClusterStatusError `json:"failureReason,omitempty"`

	// FailureMessage indicates that there is a fatal problem reconciling the
	// state, and will be set to a descriptive error message.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

	// Conditions defines current service state of the cluster.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// GardenerCluster is the Schema for the gardenerclusters API.
type GardenerCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GardenerClusterSpec   `json:"spec,omitempty"`
	Status GardenerClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GardenerClusterList contains a list of GardenerCluster.
type GardenerClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GardenerCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GardenerCluster{}, &GardenerClusterList{})
}

// GetConditions returns the list of conditions.
func (c *GardenerCluster) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions will set the given conditions.
func (c *GardenerCluster) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}
