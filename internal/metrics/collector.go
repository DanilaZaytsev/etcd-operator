/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package metrics exposes the operator's view of the clusters it manages as
// Prometheus metrics.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// Collector reports the state of every EtcdCluster, its members, and its
// backups.
//
// It is a Prometheus Collector that reads the objects at scrape time rather
// than a set of gauges written during reconcile, and that choice is the point.
// Gauges written from a reconcile keep reporting a cluster that has been
// deleted until something remembers to delete the series — and something never
// does, so a dashboard of 160 tenants slowly fills with tenants that no longer
// exist. Reading at scrape time means the metrics cannot outlive their objects:
// a deleted cluster simply stops appearing.
//
// The same property makes moderately-varying labels safe here. `reason` on a
// condition changes as a cluster moves between states, and would accumulate a
// dead series per reason if these were long-lived gauges; listed at scrape
// time, only the current one exists.
//
// The reads are served from the manager's informer cache, so a scrape is an
// in-memory walk, not a burst of apiserver traffic.
type Collector struct {
	client client.Reader

	// now is the clock, overridable in tests.
	now func() time.Time

	clusterInfo           *prometheus.Desc
	clusterBootstrapped   *prometheus.Desc
	clusterMembersDesired *prometheus.Desc
	clusterMembersReady   *prometheus.Desc
	clusterCondition      *prometheus.Desc
	clusterBrokenMembers  *prometheus.Desc
	clusterGeneration     *prometheus.Desc
	clusterObservedGen    *prometheus.Desc

	defragsByPhase        *prometheus.Desc
	defragPolicyInfo      *prometheus.Desc
	defragPolicySuspended *prometheus.Desc
	defragPolicyLastSched *prometheus.Desc
	defragPolicyLastOK    *prometheus.Desc
	defragPolicyActive    *prometheus.Desc

	memberReady *prometheus.Desc
	memberVoter *prometheus.Desc

	snapshotLastSuccessTime  *prometheus.Desc
	snapshotLastSuccessBytes *prometheus.Desc
	snapshotLastSuccessAge   *prometheus.Desc
	snapshotsByPhase         *prometheus.Desc

	policyInfo             *prometheus.Desc
	policySuspended        *prometheus.Desc
	policyLastScheduleTime *prometheus.Desc
	policyLastSuccessTime  *prometheus.Desc
	policyActive           *prometheus.Desc
}

// NewCollector builds the collector. reader should be the manager's cached
// client.
func NewCollector(reader client.Reader) *Collector {
	cluster := []string{"namespace", "cluster"}
	return &Collector{
		client: reader,
		now:    time.Now,

		clusterInfo: prometheus.NewDesc(
			"etcd_operator_cluster_info",
			"Static facts about an EtcdCluster. Always 1; read the labels.",
			[]string{"namespace", "cluster", "version", "storage_medium", "cluster_id"}, nil),
		clusterBootstrapped: prometheus.NewDesc(
			"etcd_operator_cluster_bootstrapped",
			"1 once the cluster has formed and latched an etcd cluster ID, 0 before that. "+
				"This is the 'is it actually assembled' signal: a cluster can have Pods running and "+
				"still not have formed.",
			cluster, nil),
		clusterMembersDesired: prometheus.NewDesc(
			"etcd_operator_cluster_members_desired",
			"Target member count the operator is currently reconciling toward (the latched target, "+
				"not spec.replicas, which may be ahead of it).",
			cluster, nil),
		clusterMembersReady: prometheus.NewDesc(
			"etcd_operator_cluster_members_ready",
			"Members currently healthy and serving. Compare against members_desired: "+
				"below quorum the cluster is down, below target it is degraded.",
			cluster, nil),
		clusterCondition: prometheus.NewDesc(
			"etcd_operator_cluster_condition",
			"EtcdCluster status conditions: 1 when the condition is True, 0 when False, absent when Unknown. "+
				"The reason label carries why.",
			[]string{"namespace", "cluster", "condition", "reason"}, nil),

		clusterBrokenMembers: prometheus.NewDesc(
			"etcd_operator_cluster_broken_members",
			"status.brokenMembers: members the operator considers broken rather than merely "+
				"absent. NOTE: the predicate behind this field is currently a stub for "+
				"PVC-backed clusters (see isBroken), so it reads 0 for them by construction. "+
				"It is exported because it is part of the status API and will become "+
				"meaningful if that predicate grows; do not write an alert against it today — "+
				"use members_ready against members_desired instead.",
			cluster, nil),
		clusterGeneration: prometheus.NewDesc(
			"etcd_operator_cluster_generation",
			"metadata.generation of the EtcdCluster — the spec the user has asked for.",
			cluster, nil),
		clusterObservedGen: prometheus.NewDesc(
			"etcd_operator_cluster_observed_generation",
			"status.observedGeneration — the spec the operator has finished a reconcile pass for. "+
				"Lagging behind cluster_generation means the operator has not caught up yet; "+
				"lagging for long means it is stuck, and this is the signal to alert on.",
			cluster, nil),

		defragsByPhase: prometheus.NewDesc(
			"etcd_operator_defrags",
			"EtcdDefrag objects per cluster, by phase.",
			[]string{"namespace", "cluster", "phase"}, nil),
		defragPolicyInfo: prometheus.NewDesc(
			"etcd_operator_defrag_policy_info",
			"Static facts about an EtcdDefragPolicy. Always 1; read the labels.",
			[]string{"namespace", "policy", "cluster", "schedule", "timezone"}, nil),
		defragPolicySuspended: prometheus.NewDesc(
			"etcd_operator_defrag_policy_suspended",
			"1 when the defrag policy is suspended and will not fire, 0 when it is live. "+
				"A suspended policy still exports its other series, so a paused schedule is "+
				"visible rather than silently absent.",
			[]string{"namespace", "policy"}, nil),
		defragPolicyLastSched: prometheus.NewDesc(
			"etcd_operator_defrag_policy_last_schedule_timestamp_seconds",
			"Unix time the defrag policy last fired a tick.",
			[]string{"namespace", "policy"}, nil),
		defragPolicyLastOK: prometheus.NewDesc(
			"etcd_operator_defrag_policy_last_success_timestamp_seconds",
			"Unix time a defrag started by this policy last completed successfully.",
			[]string{"namespace", "policy"}, nil),
		defragPolicyActive: prometheus.NewDesc(
			"etcd_operator_defrag_policy_active_defrags",
			"EtcdDefrag objects this policy currently has in flight.",
			[]string{"namespace", "policy"}, nil),

		memberReady: prometheus.NewDesc(
			"etcd_operator_member_ready",
			"1 when an individual EtcdMember is healthy and serving.",
			[]string{"namespace", "cluster", "member", "storage_pool"}, nil),
		memberVoter: prometheus.NewDesc(
			"etcd_operator_member_voter",
			"1 when a member votes in the raft quorum, 0 while it is still a learner.",
			[]string{"namespace", "cluster", "member"}, nil),

		snapshotLastSuccessTime: prometheus.NewDesc(
			"etcd_operator_snapshot_last_success_timestamp_seconds",
			"When the most recent snapshot of this cluster was successfully stored, as a Unix timestamp. "+
				"Absent for a cluster that has never had one — alert on absence as well as on age.",
			cluster, nil),
		snapshotLastSuccessAge: prometheus.NewDesc(
			"etcd_operator_snapshot_last_success_age_seconds",
			"Seconds since the most recent successful snapshot of this cluster. The same fact as the "+
				"timestamp above, pre-subtracted so an alert does not have to know the scrape time.",
			cluster, nil),
		snapshotLastSuccessBytes: prometheus.NewDesc(
			"etcd_operator_snapshot_last_success_size_bytes",
			"Size of the most recent successfully stored snapshot. A collapse here is the signal that "+
				"backups are running but capturing nothing.",
			cluster, nil),
		snapshotsByPhase: prometheus.NewDesc(
			"etcd_operator_snapshots",
			"EtcdSnapshot objects for this cluster, by phase. Retained history, not a rate — "+
				"the count is bounded by the policy's history limits.",
			[]string{"namespace", "cluster", "phase"}, nil),

		policyInfo: prometheus.NewDesc(
			"etcd_operator_snapshot_policy_info",
			"Static facts about an EtcdSnapshotPolicy, including the schedule it actually runs on "+
				"(which the chart may have spread across the hour). Always 1; read the labels.",
			[]string{"namespace", "policy", "cluster", "schedule", "timezone"}, nil),
		policySuspended: prometheus.NewDesc(
			"etcd_operator_snapshot_policy_suspended",
			"1 while a backup policy is suspended. A suspended policy is not a failing one, so it will "+
				"not show up in any staleness alert — which is exactly why it needs its own series.",
			[]string{"namespace", "policy"}, nil),
		policyLastScheduleTime: prometheus.NewDesc(
			"etcd_operator_snapshot_policy_last_schedule_timestamp_seconds",
			"When this policy last acted on a scheduled tick, as a Unix timestamp. Diverging from "+
				"last_success means snapshots are being stamped and then failing.",
			[]string{"namespace", "policy"}, nil),
		policyLastSuccessTime: prometheus.NewDesc(
			"etcd_operator_snapshot_policy_last_success_timestamp_seconds",
			"When a snapshot stamped by this policy last completed, as a Unix timestamp.",
			[]string{"namespace", "policy"}, nil),
		policyActive: prometheus.NewDesc(
			"etcd_operator_snapshot_policy_active_snapshots",
			"Snapshots stamped by this policy that have not finished yet.",
			[]string{"namespace", "policy"}, nil),
	}
}

// Register adds the collector to controller-runtime's registry, so it is served
// on the manager's existing /metrics endpoint rather than a second listener.
func (c *Collector) Register() error {
	return ctrlmetrics.Registry.Register(c)
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.clusterInfo, c.clusterBootstrapped, c.clusterMembersDesired, c.clusterMembersReady,
		c.clusterCondition, c.clusterBrokenMembers, c.clusterGeneration, c.clusterObservedGen,
		c.memberReady, c.memberVoter,
		c.defragsByPhase, c.defragPolicyInfo, c.defragPolicySuspended,
		c.defragPolicyLastSched, c.defragPolicyLastOK, c.defragPolicyActive,
		c.snapshotLastSuccessTime, c.snapshotLastSuccessAge, c.snapshotLastSuccessBytes, c.snapshotsByPhase,
		c.policyInfo, c.policySuspended, c.policyLastScheduleTime, c.policyLastSuccessTime, c.policyActive,
	} {
		ch <- d
	}
}

// Collect walks the operator's objects once per scrape.
//
// Errors are swallowed rather than surfaced as a failed scrape: a transient
// cache read that fails should leave the affected metrics missing for one
// interval, not blank the whole endpoint — the manager's own controller-runtime
// metrics are served from the same registry and are how you would diagnose it.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	var clusters lll.EtcdClusterList
	if err := c.client.List(ctx, &clusters); err != nil {
		return
	}
	for i := range clusters.Items {
		c.collectCluster(ch, &clusters.Items[i])
	}

	var members lll.EtcdMemberList
	if err := c.client.List(ctx, &members); err == nil {
		for i := range members.Items {
			c.collectMember(ch, &members.Items[i])
		}
	}

	var snapshots lll.EtcdSnapshotList
	if err := c.client.List(ctx, &snapshots); err == nil {
		c.collectSnapshots(ch, snapshots.Items)
	}

	var policies lll.EtcdSnapshotPolicyList
	if err := c.client.List(ctx, &policies); err == nil {
		for i := range policies.Items {
			c.collectPolicy(ch, &policies.Items[i])
		}
	}

	var defrags lll.EtcdDefragList
	if err := c.client.List(ctx, &defrags); err == nil {
		c.collectDefrags(ch, defrags.Items)
	}

	var defragPolicies lll.EtcdDefragPolicyList
	if err := c.client.List(ctx, &defragPolicies); err == nil {
		for i := range defragPolicies.Items {
			c.collectDefragPolicy(ch, &defragPolicies.Items[i])
		}
	}
}

func (c *Collector) collectCluster(ch chan<- prometheus.Metric, cl *lll.EtcdCluster) {
	ns, name := cl.Namespace, cl.Name

	medium := string(cl.Spec.Storage.Medium)
	if medium == "" {
		medium = "PersistentVolumeClaim"
	}
	gauge(ch, c.clusterInfo, 1, ns, name, cl.Spec.Version, medium, cl.Status.ClusterID)

	// "Assembled" is the clusterID being latched, not Pods existing: a cluster
	// whose members are running but which never formed has no cluster ID.
	bootstrapped := 0.0
	if cl.Status.ClusterID != "" {
		bootstrapped = 1
	}
	gauge(ch, c.clusterBootstrapped, bootstrapped, ns, name)

	// The latched target, not spec.replicas: the operator reconciles toward the
	// former, and a metric reporting the latter would show "ready 3 / desired 5"
	// the instant someone edited the spec, before the operator had accepted it.
	desired := 0.0
	if cl.Status.Observed != nil {
		desired = float64(cl.Status.Observed.Replicas)
	} else if cl.Spec.Replicas != nil {
		desired = float64(*cl.Spec.Replicas)
	}
	gauge(ch, c.clusterMembersDesired, desired, ns, name)
	gauge(ch, c.clusterMembersReady, float64(cl.Status.ReadyMembers), ns, name)

	gauge(ch, c.clusterBrokenMembers, float64(cl.Status.BrokenMembers), ns, name)

	// Exported as two series rather than one difference so the staleness alert
	// can be written as a comparison the reader can verify against kubectl, and
	// so a cluster whose spec never changed does not look stuck.
	gauge(ch, c.clusterGeneration, float64(cl.Generation), ns, name)
	gauge(ch, c.clusterObservedGen, float64(cl.Status.ObservedGeneration), ns, name)

	for _, cond := range cl.Status.Conditions {
		v, ok := conditionValue(cond.Status)
		if !ok {
			continue
		}
		gauge(ch, c.clusterCondition, v, ns, name, cond.Type, cond.Reason)
	}
}

func (c *Collector) collectMember(ch chan<- prometheus.Metric, m *lll.EtcdMember) {
	ready := 0.0
	for _, cond := range m.Status.Conditions {
		if cond.Type == lll.MemberReady && cond.Status == metav1.ConditionTrue {
			ready = 1
			break
		}
	}
	gauge(ch, c.memberReady, ready, m.Namespace, m.Spec.ClusterName, m.Name, m.Spec.StoragePool)

	voter := 0.0
	if m.Status.IsVoter {
		voter = 1
	}
	gauge(ch, c.memberVoter, voter, m.Namespace, m.Spec.ClusterName, m.Name)
}

// collectSnapshots reports per-cluster backup freshness.
//
// Keyed on the cluster rather than on the policy on purpose: "has this tenant
// been backed up recently" is the question worth alerting on, and it has the
// same answer whether the snapshot came from a policy, from a one-shot
// EtcdSnapshot, or from a policy that has since been deleted.
func (c *Collector) collectSnapshots(ch chan<- prometheus.Metric, items []lll.EtcdSnapshot) {
	type clusterKey struct{ ns, cluster string }
	type latest struct {
		when  time.Time
		bytes int64
		found bool
	}
	best := map[clusterKey]*latest{}
	counts := map[clusterKey]map[string]int{}

	for i := range items {
		s := &items[i]
		key := clusterKey{s.Namespace, s.Spec.ClusterRef.Name}

		phase := string(s.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		if counts[key] == nil {
			counts[key] = map[string]int{}
		}
		counts[key][phase]++

		if s.Status.Phase != lll.EtcdSnapshotStatusPhaseComplete || s.Status.Artifact == nil {
			continue
		}
		when := completionTime(s)
		cur := best[key]
		if cur == nil || when.After(cur.when) {
			best[key] = &latest{when: when, bytes: s.Status.Artifact.SizeBytes, found: true}
		}
	}

	for key, counted := range counts {
		for phase, n := range counted {
			gauge(ch, c.snapshotsByPhase, float64(n), key.ns, key.cluster, phase)
		}
	}
	for key, l := range best {
		if !l.found {
			continue
		}
		gauge(ch, c.snapshotLastSuccessTime, float64(l.when.Unix()), key.ns, key.cluster)
		gauge(ch, c.snapshotLastSuccessAge, c.now().Sub(l.when).Seconds(), key.ns, key.cluster)
		if l.bytes > 0 {
			gauge(ch, c.snapshotLastSuccessBytes, float64(l.bytes), key.ns, key.cluster)
		}
	}
}

func (c *Collector) collectPolicy(ch chan<- prometheus.Metric, p *lll.EtcdSnapshotPolicy) {
	ns, name := p.Namespace, p.Name
	gauge(ch, c.policyInfo, 1, ns, name, p.Spec.ClusterRef.Name, p.Spec.Schedule.Cron, p.Spec.Schedule.Timezone)

	suspended := 0.0
	if p.Spec.Suspend != nil && *p.Spec.Suspend {
		suspended = 1
	}
	gauge(ch, c.policySuspended, suspended, ns, name)

	if t := p.Status.LastScheduleTime; t != nil {
		gauge(ch, c.policyLastScheduleTime, float64(t.Unix()), ns, name)
	}
	if t := p.Status.LastSuccessfulTime; t != nil {
		gauge(ch, c.policyLastSuccessTime, float64(t.Unix()), ns, name)
	}
	gauge(ch, c.policyActive, float64(len(p.Status.Active)), ns, name)
}

// collectDefrags counts EtcdDefrag objects per cluster by phase. Defrag is the
// one maintenance operation the operator performs against a live cluster, and
// until now it exported nothing at all — a policy that stopped firing, or one
// whose defrags kept failing, was invisible to monitoring while the snapshot
// side of the same operator was fully instrumented.
func (c *Collector) collectDefrags(ch chan<- prometheus.Metric, items []lll.EtcdDefrag) {
	type clusterKey struct{ ns, cluster string }
	counts := map[clusterKey]map[string]int{}

	for i := range items {
		d := &items[i]
		key := clusterKey{d.Namespace, d.Spec.ClusterRef.Name}
		phase := string(d.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		if counts[key] == nil {
			counts[key] = map[string]int{}
		}
		counts[key][phase]++
	}

	for key, byPhase := range counts {
		for phase, n := range byPhase {
			gauge(ch, c.defragsByPhase, float64(n), key.ns, key.cluster, phase)
		}
	}
}

func (c *Collector) collectDefragPolicy(ch chan<- prometheus.Metric, p *lll.EtcdDefragPolicy) {
	ns, name := p.Namespace, p.Name
	gauge(ch, c.defragPolicyInfo, 1, ns, name, p.Spec.ClusterRef.Name, p.Spec.Schedule.Cron, p.Spec.Schedule.Timezone)

	suspended := 0.0
	if p.Spec.Suspend != nil && *p.Spec.Suspend {
		suspended = 1
	}
	gauge(ch, c.defragPolicySuspended, suspended, ns, name)

	if t := p.Status.LastScheduleTime; t != nil {
		gauge(ch, c.defragPolicyLastSched, float64(t.Unix()), ns, name)
	}
	if t := p.Status.LastSuccessfulTime; t != nil {
		gauge(ch, c.defragPolicyLastOK, float64(t.Unix()), ns, name)
	}
	gauge(ch, c.defragPolicyActive, float64(len(p.Status.Active)), ns, name)
}

// completionTime is when a snapshot reached its terminal phase. EtcdSnapshot
// records no completion timestamp of its own, so the Ready condition's last
// transition is the finish time, falling back to creation.
func completionTime(s *lll.EtcdSnapshot) time.Time {
	for i := range s.Status.Conditions {
		if s.Status.Conditions[i].Type == lll.SnapshotReady {
			if t := s.Status.Conditions[i].LastTransitionTime; !t.IsZero() {
				return t.Time
			}
		}
	}
	return s.CreationTimestamp.Time
}

// conditionValue maps a condition status to a gauge value. Unknown yields no
// metric at all rather than a third value: a condition nobody has evaluated is
// not the same as one evaluated to false, and folding them together would let
// "we do not know yet" satisfy an alert written against 0.
func conditionValue(s metav1.ConditionStatus) (float64, bool) {
	switch s {
	case metav1.ConditionTrue:
		return 1, true
	case metav1.ConditionFalse:
		return 0, true
	default:
		return 0, false
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}
