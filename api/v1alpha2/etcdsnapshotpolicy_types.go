/*
Copyright 2023 Timofey Larkin.

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

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EtcdSnapshotPolicySpec is the desired state of an EtcdSnapshotPolicy: a
// recurring schedule that stamps out EtcdSnapshot runs against one EtcdCluster.
//
// EtcdSnapshot is one-shot by design, which leaves every cluster's backup
// cadence to something outside the operator — an external CronJob, or nothing.
// For a parent cluster hosting many tenant control planes "or nothing" is the
// common outcome, and the first time anyone notices is during a restore. This
// puts the cadence where the cluster is.
//
// Deliberately the same shape as EtcdDefragPolicy (schedule, suspend,
// concurrency, starting deadline, history) — one scheduling model to learn, and
// the two share their scheduling helpers.
// +kubebuilder:validation:XValidation:rule="size(self.clusterRef.name) != 0",message="spec.clusterRef.name is required"
type EtcdSnapshotPolicySpec struct {
	// ClusterRef names the EtcdCluster (same namespace) each stamped
	// EtcdSnapshot targets.
	ClusterRef corev1.LocalObjectReference `json:"clusterRef"`

	// Schedule names when a snapshot is stamped.
	Schedule DefragSchedule `json:"schedule"`

	// Destination is stamped verbatim into each EtcdSnapshot. For an S3
	// destination the key is a prefix and each stamped snapshot appends its own
	// name, so runs never overwrite one another.
	Destination SnapshotLocation `json:"destination"`

	// Suspend pauses stamping. Snapshots already in flight are left alone. On
	// resume the single most recent missed tick may be stamped (subject to
	// StartingDeadlineSeconds); earlier missed ticks are never replayed.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy decides what a due tick does when a previous stamped
	// snapshot is still running. Defaults to Forbid: a snapshot that outruns its
	// own schedule (a large keyspace on a slow link) would otherwise pile up
	// concurrent streams against the same cluster.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds bounds how late a missed tick may still be
	// started. If the operator was down and more than this many seconds have
	// passed since the scheduled time, that tick is skipped rather than started
	// late. Absent means no deadline.
	//
	// Capped at ten years for the same reason as EtcdDefragPolicy's: the value
	// is multiplied out to a time.Duration, which overflows past ~292 years and
	// wraps to a negative window that silently suppresses every tick.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=315360000
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// SuccessfulHistoryLimit caps how many Complete EtcdSnapshots stamped by
	// this policy are retained; the oldest beyond the limit are deleted.
	// Defaults to 10.
	//
	// Whether pruning the object also deletes the stored snapshot it points
	// at is decided by DeleteArtifactOnPrune.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +optional
	SuccessfulHistoryLimit *int32 `json:"successfulHistoryLimit,omitempty"`

	// FailedHistoryLimit caps how many Failed EtcdSnapshots are retained.
	// Defaults to 3 — enough to see a pattern, few enough not to bury the
	// successes. Failed runs never have an artifact to prune.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedHistoryLimit *int32 `json:"failedHistoryLimit,omitempty"`

	// DeleteArtifactOnPrune makes history GC delete the stored snapshot along
	// with the EtcdSnapshot object it prunes, so the destination does not grow
	// without bound.
	//
	// Off by default: deleting backups is not something to start doing because
	// a retention limit defaulted.
	//
	// Only ever deletes the artifact recorded in the pruned EtcdSnapshot's
	// status.artifact, one object per pruned snapshot. The operator never lists
	// the destination and never deletes by prefix or age — a bucket is
	// frequently shared, and "delete everything older than N under this prefix"
	// is how a retention policy eats somebody else's backups.
	//
	// A snapshot whose artifact cannot be deleted is kept, not silently
	// dropped: losing track of a stored object is worse than retaining one
	// EtcdSnapshot past its limit.
	// +optional
	DeleteArtifactOnPrune bool `json:"deleteArtifactOnPrune,omitempty"`
}

// EtcdSnapshotPolicyStatus is the observed state of an EtcdSnapshotPolicy.
type EtcdSnapshotPolicyStatus struct {
	// LastScheduleTime is the scheduled time of the most recent tick the policy
	// stamped a snapshot for, or consumed by skipping under ConcurrencyPolicy.
	// It anchors the next tick, so a tick is never acted on twice.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is when a stamped snapshot most recently reached
	// Complete.
	//
	// This is the field to alert on. "No successful backup in N hours" is the
	// question that matters, and answering it from here is one Get rather than
	// a list-and-filter over every EtcdSnapshot in the namespace.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// LastSuccessfulArtifact records where the most recent successful snapshot
	// was stored, so a restore can be pointed at it without hunting through the
	// stamped EtcdSnapshots.
	// +optional
	LastSuccessfulArtifact *SnapshotArtifact `json:"lastSuccessfulArtifact,omitempty"`

	// Active references the stamped EtcdSnapshots that have not yet finished.
	// +optional
	// +listType=atomic
	Active []corev1.LocalObjectReference `json:"active,omitempty"`

	// Conditions represent the latest observations — notably why stamping is
	// paused (Suspended) or not happening (InvalidSchedule).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:resource:shortName=etcdsp,categories=etcd
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// Capped tighter than EtcdDefragPolicy's 52, because this name is the head of
// a longer derivation chain. It is a label value on each stamped EtcdSnapshot
// (63 max, or the controller's own selector is unparseable), the snapshot is
// named "<policy>-<tick>" where the tick is 8 digits, and the snapshot
// controller then names its Job "<snapshot>-snapshot" — which the apiserver
// rejects above 63 characters, because the Job controller copies the name into
// a Pod label.
//
//	44 + 1 + 8 + len("-snapshot") = 62
//
// At the old 52 that arithmetic gives 70: the policy would stamp snapshots
// perfectly well and every one of them would fail to run, with the error two
// objects away from the name that caused it. 44 rather than the exact 45
// leaves a digit of headroom for the tick.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 44",message="metadata.name must be 44 characters or fewer: it becomes part of each stamped EtcdSnapshot's name, and of the Job name derived from that, which the apiserver caps at 63"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule.cron`
// +kubebuilder:printcolumn:name="Timezone",type=string,JSONPath=`.spec.schedule.timezone`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last Schedule",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Last Successful",type=date,JSONPath=`.status.lastSuccessfulTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EtcdSnapshotPolicy is the Schema for the etcdsnapshotpolicies API. It stamps
// out EtcdSnapshot runs on a cron schedule so the operator drives recurring
// backups itself. Each run is a discrete, auditable EtcdSnapshot owned by the
// policy.
type EtcdSnapshotPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdSnapshotPolicySpec   `json:"spec,omitempty"`
	Status EtcdSnapshotPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdSnapshotPolicyList contains a list of EtcdSnapshotPolicy.
type EtcdSnapshotPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdSnapshotPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdSnapshotPolicy{}, &EtcdSnapshotPolicyList{})
}
