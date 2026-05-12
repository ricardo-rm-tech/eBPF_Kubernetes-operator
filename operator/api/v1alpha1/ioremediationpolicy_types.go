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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// IORemediationPolicySpec defines the desired state of IORemediationPolicy
type IORemediationPolicySpec struct {
	// TargetPodSelector defines which Pods this policy applies to.
	TargetPodSelector metav1.LabelSelector `json:"targetPodSelector"`

	// PrometheusEndpoint defines where to query the metrics.
	PrometheusEndpoint string `json:"prometheusEndpoint"`

	// MetricType defines if we are measuring IO or CPU saturation.
	// Options: "IO", "CPU"
	// +kubebuilder:validation:Enum=IO;CPU
	// +kubebuilder:default:=IO
	// +optional
	MetricType string `json:"metricType,omitempty"`

	// LatencyThreshold defines the saturation limits.
	// E.g., "50ms" average I/O or RunQ latency
	LatencyThreshold string `json:"latencyThreshold"`

	// EvaluationWindow is the period over which the latency is evaluated (e.g. "5m")
	EvaluationWindow string `json:"evaluationWindow"`

	// Action determines what to do in case of saturation.
	// Options: "EvictAndTaint", "MigrateStorageClass", "ScaleUp"
	// +kubebuilder:validation:Enum=EvictAndTaint;MigrateStorageClass;ScaleUp
	Action string `json:"action"`

	// EvictAndTaintConfig is used if Action == "EvictAndTaint"
	// +optional
	EvictAndTaintConfig *EvictAndTaintOptions `json:"evictAndTaintConfig,omitempty"`

	// MigrateStorageConfig is used if Action == "MigrateStorageClass"
	// +optional
	MigrateStorageConfig *MigrateStorageOptions `json:"migrateStorageConfig,omitempty"`

	// ScaleUpConfig is used if Action == "ScaleUp"
	// +optional
	ScaleUpConfig *ScaleUpOptions `json:"scaleUpConfig,omitempty"`
}

type EvictAndTaintOptions struct {
	// TaintDuration specifies how long the taint should remain on the node.
	// If empty, it might be permanent until manual intervention.
	// +optional
	TaintDuration *metav1.Duration `json:"taintDuration,omitempty"`
}

type MigrateStorageOptions struct {
	// TargetStorageClass is the fast storage class to migrate to (e.g., "nvme-ssd")
	TargetStorageClass string `json:"targetStorageClass"`
}

type ScaleUpOptions struct {
	// CPUStepPercent specifies the percentage increase per step (e.g., "20")
	CPUStepPercent int32 `json:"cpuStepPercent"`
	// MaxCPULimit specifies the absolute maximum CPU limit allowed (e.g., "2000m")
	MaxCPULimit string `json:"maxCpuLimit"`
}

// IORemediationPolicyStatus defines the observed state of IORemediationPolicy.
type IORemediationPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the IORemediationPolicy resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// IORemediationPolicy is the Schema for the ioremediationpolicies API
type IORemediationPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IORemediationPolicy
	// +required
	Spec IORemediationPolicySpec `json:"spec"`

	// status defines the observed state of IORemediationPolicy
	// +optional
	Status IORemediationPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IORemediationPolicyList contains a list of IORemediationPolicy
type IORemediationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IORemediationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IORemediationPolicy{}, &IORemediationPolicyList{})
}
