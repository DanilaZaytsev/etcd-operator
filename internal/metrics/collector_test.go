/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// These assert the questions the metrics exist to answer, not the shape of the
// exposition: is the cluster actually assembled, how far is it from its target,
// and when was it last backed up. A metric that is present but answers a
// slightly different question is worse than a missing one, because an alert
// gets written against it.

var epoch = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func newCollector(t *testing.T, objs ...client.Object) *Collector {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := lll.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build())
	c.now = func() time.Time { return epoch }
	return c
}

// samples gathers the collector into a flat list of (name, labels, value), which
// is what the assertions below actually care about. Comparing rendered text
// would drag float formatting into every assertion.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

func gather(t *testing.T, c *Collector) []sample {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []sample
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out = append(out, sample{f.GetName(), labels, valueOf(m)})
		}
	}
	return out
}

func valueOf(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return 0
}

// find returns the single sample matching name and every given label, or fails.
func find(t *testing.T, ss []sample, name string, labels map[string]string) sample {
	t.Helper()
	var hits []sample
	for _, s := range ss {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, s)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one %s%v, got %d: %v", name, labels, len(hits), hits)
	}
	return hits[0]
}

func absent(t *testing.T, ss []sample, name string, labels map[string]string) {
	t.Helper()
	for _, s := range ss {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range labels {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			t.Fatalf("%s%v should not be reported, got value %v", name, labels, s.value)
		}
	}
}

func cluster(name string, opts ...func(*lll.EtcdCluster)) *lll.EtcdCluster {
	three := int32(3)
	c := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a"},
		Spec:       lll.EtcdClusterSpec{Replicas: &three, Version: "3.6.11"},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func inCluster(name string) map[string]string {
	return map[string]string{"namespace": "tenant-a", "cluster": name}
}

// "Is the cluster assembled" is the clusterID being latched, not Pods running.
// A cluster whose members are up but which never formed has no cluster ID, and
// reporting it as assembled would hide exactly the failure worth catching.
func TestClusterBootstrapped(t *testing.T) {
	forming := cluster("forming")
	formed := cluster("formed", func(c *lll.EtcdCluster) {
		c.Status.ClusterID = "abcd1234"
		c.Status.ReadyMembers = 3
		c.Status.Observed = &lll.ObservedClusterSpec{Replicas: 3, Version: "3.6.11"}
	})

	ss := gather(t, newCollector(t, forming, formed))

	if got := find(t, ss, "etcd_operator_cluster_bootstrapped", inCluster("formed")).value; got != 1 {
		t.Fatalf("a formed cluster reports bootstrapped=%v, want 1", got)
	}
	if got := find(t, ss, "etcd_operator_cluster_bootstrapped", inCluster("forming")).value; got != 0 {
		t.Fatalf("a cluster that never formed reports bootstrapped=%v, want 0", got)
	}
	// The cluster ID is a label so "which incarnation is this" is answerable
	// after a restore, which assigns a new one.
	if got := find(t, ss, "etcd_operator_cluster_info", inCluster("formed")).labels["cluster_id"]; got != "abcd1234" {
		t.Fatalf("cluster_id label = %q", got)
	}
}

// The target has to be the LATCHED one. Reporting spec.replicas would show
// "ready 3 / desired 5" the instant someone edited the spec, before the
// operator had accepted the new target — a degraded reading for a healthy
// cluster, which is how an alert learns to be ignored.
func TestClusterMembersDesiredUsesTheLatchedTarget(t *testing.T) {
	five := int32(5)
	c := cluster("scaling", func(c *lll.EtcdCluster) {
		c.Spec.Replicas = &five // the user just asked for 5
		c.Status.ReadyMembers = 3
		c.Status.Observed = &lll.ObservedClusterSpec{Replicas: 3} // operator still on 3
	})

	ss := gather(t, newCollector(t, c))

	if got := find(t, ss, "etcd_operator_cluster_members_desired", inCluster("scaling")).value; got != 3 {
		t.Fatalf("desired = %v, want the latched 3 rather than spec's 5", got)
	}
	if got := find(t, ss, "etcd_operator_cluster_members_ready", inCluster("scaling")).value; got != 3 {
		t.Fatalf("ready = %v, want 3", got)
	}
}

func TestClusterConditions(t *testing.T) {
	c := cluster("c1", func(c *lll.EtcdCluster) {
		c.Status.Conditions = []metav1.Condition{
			{Type: lll.ClusterAvailable, Status: metav1.ConditionTrue, Reason: "QuorumHealthy"},
			{Type: lll.ClusterDegraded, Status: metav1.ConditionFalse, Reason: "AllMembersReady"},
			// Unknown must produce no series at all: "nobody has evaluated this"
			// is not "this is false", and folding them together would let an
			// unevaluated condition satisfy an alert written against 0.
			{Type: lll.ClusterProgressing, Status: metav1.ConditionUnknown, Reason: "Pending"},
		}
	})

	ss := gather(t, newCollector(t, c))

	avail := find(t, ss, "etcd_operator_cluster_condition", map[string]string{
		"namespace": "tenant-a", "cluster": "c1", "condition": lll.ClusterAvailable})
	if avail.value != 1 || avail.labels["reason"] != "QuorumHealthy" {
		t.Fatalf("Available condition = %v (reason %q)", avail.value, avail.labels["reason"])
	}
	degraded := find(t, ss, "etcd_operator_cluster_condition", map[string]string{
		"namespace": "tenant-a", "cluster": "c1", "condition": lll.ClusterDegraded})
	if degraded.value != 0 {
		t.Fatalf("a False condition reports %v, want 0", degraded.value)
	}
	absent(t, ss, "etcd_operator_cluster_condition", map[string]string{"condition": lll.ClusterProgressing})
}

func TestMemberMetrics(t *testing.T) {
	voter := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "c1-4kx9", Namespace: "tenant-a"},
		Spec:       lll.EtcdMemberSpec{ClusterName: "c1", StoragePool: "array-b"},
		Status: lll.EtcdMemberStatus{
			IsVoter:    true,
			Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady"}},
		},
	}
	learner := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "c1-p2mn", Namespace: "tenant-a"},
		Spec:       lll.EtcdMemberSpec{ClusterName: "c1", StoragePool: "array-c"},
	}

	ss := gather(t, newCollector(t, voter, learner))

	// The storage pool is a label because "which array is this member on" is the
	// question during an array outage, and it should not require a join.
	ready := find(t, ss, "etcd_operator_member_ready", map[string]string{"member": "c1-4kx9"})
	if ready.value != 1 || ready.labels["storage_pool"] != "array-b" {
		t.Fatalf("ready member = %v, pool %q", ready.value, ready.labels["storage_pool"])
	}
	if got := find(t, ss, "etcd_operator_member_ready", map[string]string{"member": "c1-p2mn"}).value; got != 0 {
		t.Fatalf("a member with no Ready condition reports %v, want 0", got)
	}
	if got := find(t, ss, "etcd_operator_member_voter", map[string]string{"member": "c1-p2mn"}).value; got != 0 {
		t.Fatalf("learner reports voter=%v, want 0", got)
	}
}

func snapshot(name string, phase lll.EtcdSnapshotStatusPhase, at time.Time, size int64) *lll.EtcdSnapshot {
	s := &lll.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a", CreationTimestamp: metav1.NewTime(at)},
		Spec:       lll.EtcdSnapshotSpec{ClusterRef: corev1.LocalObjectReference{Name: "c1"}},
		Status: lll.EtcdSnapshotStatus{
			Phase: phase,
			Conditions: []metav1.Condition{{
				Type: lll.SnapshotReady, Status: metav1.ConditionFalse,
				Reason: "Test", LastTransitionTime: metav1.NewTime(at),
			}},
		},
	}
	if phase == lll.EtcdSnapshotStatusPhaseComplete {
		s.Status.Conditions[0].Status = metav1.ConditionTrue
		s.Status.Artifact = &lll.SnapshotArtifact{URI: "s3://b/" + name + ".db", SizeBytes: size}
	}
	return s
}

// "When was this tenant last backed up" must answer from the newest SUCCESS,
// not the newest snapshot: a cluster whose backups started failing an hour ago
// still has a recent Failed object, and reporting that as freshness would hide
// the outage the metric exists to catch.
func TestSnapshotFreshnessTracksTheLatestSuccess(t *testing.T) {
	older := snapshot("s1", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(-6*time.Hour), 1024)
	newer := snapshot("s2", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(-2*time.Hour), 4096)
	failedSince := snapshot("s3", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(-30*time.Minute), 0)

	ss := gather(t, newCollector(t, older, newer, failedSince))

	wantTS := float64(epoch.Add(-2 * time.Hour).Unix())
	if got := find(t, ss, "etcd_operator_snapshot_last_success_timestamp_seconds", inCluster("c1")).value; got != wantTS {
		t.Fatalf("freshness = %v, want the newest SUCCESS at %v", got, wantTS)
	}
	if got := find(t, ss, "etcd_operator_snapshot_last_success_age_seconds", inCluster("c1")).value; got != 7200 {
		t.Fatalf("age = %v seconds, want 7200", got)
	}
	if got := find(t, ss, "etcd_operator_snapshot_last_success_size_bytes", inCluster("c1")).value; got != 4096 {
		t.Fatalf("size = %v, want the newest success's 4096", got)
	}
}

// A cluster that has never been backed up must emit NO freshness series, not a
// zero. Zero is a valid Unix timestamp and an age of zero reads as "just backed
// up" — the exact inversion of the truth. Absence is what an alert catches with
// absent().
func TestNeverBackedUpEmitsNoFreshness(t *testing.T) {
	pending := snapshot("s1", lll.EtcdSnapshotStatusPhasePending, epoch, 0)

	ss := gather(t, newCollector(t, cluster("c1"), pending))

	absent(t, ss, "etcd_operator_snapshot_last_success_timestamp_seconds", inCluster("c1"))
	absent(t, ss, "etcd_operator_snapshot_last_success_age_seconds", inCluster("c1"))
	// ...but the snapshot itself is still counted, so "it is trying" is visible.
	if got := find(t, ss, "etcd_operator_snapshots", map[string]string{
		"namespace": "tenant-a", "cluster": "c1", "phase": "Pending"}).value; got != 1 {
		t.Fatalf("pending snapshot not counted: %v", got)
	}
}

func TestSnapshotCountsByPhase(t *testing.T) {
	objs := []client.Object{
		snapshot("s1", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(-3*time.Hour), 1),
		snapshot("s2", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(-2*time.Hour), 1),
		snapshot("s3", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(-time.Hour), 0),
	}
	ss := gather(t, newCollector(t, objs...))

	if got := find(t, ss, "etcd_operator_snapshots", map[string]string{"cluster": "c1", "phase": "Complete"}).value; got != 2 {
		t.Fatalf("Complete count = %v, want 2", got)
	}
	if got := find(t, ss, "etcd_operator_snapshots", map[string]string{"cluster": "c1", "phase": "Failed"}).value; got != 1 {
		t.Fatalf("Failed count = %v, want 1", got)
	}
}

func TestPolicyMetrics(t *testing.T) {
	suspend := true
	p := &lll.EtcdSnapshotPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "c1-backup", Namespace: "tenant-a"},
		Spec: lll.EtcdSnapshotPolicySpec{
			ClusterRef: corev1.LocalObjectReference{Name: "c1"},
			Schedule:   lll.DefragSchedule{Cron: "37 */6 * * *", Timezone: "Europe/Moscow"},
			Suspend:    &suspend,
		},
		Status: lll.EtcdSnapshotPolicyStatus{
			LastScheduleTime:   &metav1.Time{Time: epoch.Add(-time.Hour)},
			LastSuccessfulTime: &metav1.Time{Time: epoch.Add(-time.Hour)},
			Active:             []corev1.LocalObjectReference{{Name: "c1-backup-1"}},
		},
	}

	ss := gather(t, newCollector(t, p))
	byPolicy := map[string]string{"namespace": "tenant-a", "policy": "c1-backup"}

	// The schedule label carries the cron the policy actually runs on, which the
	// chart may have spread across the hour — otherwise "why did this fire at
	// :37" has no answer from monitoring alone.
	info := find(t, ss, "etcd_operator_snapshot_policy_info", byPolicy)
	if info.labels["schedule"] != "37 */6 * * *" || info.labels["timezone"] != "Europe/Moscow" {
		t.Fatalf("policy info labels = %v", info.labels)
	}
	if got := find(t, ss, "etcd_operator_snapshot_policy_suspended", byPolicy).value; got != 1 {
		t.Fatalf("suspended = %v, want 1", got)
	}
	if got := find(t, ss, "etcd_operator_snapshot_policy_active_snapshots", byPolicy).value; got != 1 {
		t.Fatalf("active = %v, want 1", got)
	}
	wantTS := float64(epoch.Add(-time.Hour).Unix())
	if got := find(t, ss, "etcd_operator_snapshot_policy_last_success_timestamp_seconds", byPolicy).value; got != wantTS {
		t.Fatalf("last success = %v, want %v", got, wantTS)
	}
}

// A policy that has never fired must not report a schedule or success time of
// zero — same reasoning as the freshness metrics.
func TestPolicyWithNoHistoryEmitsNoTimestamps(t *testing.T) {
	p := &lll.EtcdSnapshotPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "tenant-a"},
		Spec: lll.EtcdSnapshotPolicySpec{
			ClusterRef: corev1.LocalObjectReference{Name: "c1"},
			Schedule:   lll.DefragSchedule{Cron: "0 * * * *"},
		},
	}
	ss := gather(t, newCollector(t, p))
	byPolicy := map[string]string{"policy": "fresh"}

	absent(t, ss, "etcd_operator_snapshot_policy_last_schedule_timestamp_seconds", byPolicy)
	absent(t, ss, "etcd_operator_snapshot_policy_last_success_timestamp_seconds", byPolicy)
	if got := find(t, ss, "etcd_operator_snapshot_policy_suspended", byPolicy).value; got != 0 {
		t.Fatalf("an unsuspended policy reports %v", got)
	}
}

// The whole reason this is a Collector and not a set of reconcile-written
// gauges: a deleted cluster stops being reported, with nothing having to
// remember to delete its series.
func TestDeletedObjectsStopBeingReported(t *testing.T) {
	ss := gather(t, newCollector(t, cluster("gone")))
	find(t, ss, "etcd_operator_cluster_info", inCluster("gone"))

	ss = gather(t, newCollector(t))
	absent(t, ss, "etcd_operator_cluster_info", inCluster("gone"))
}
