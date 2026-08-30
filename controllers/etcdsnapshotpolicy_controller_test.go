/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// What these tests protect is not the cron arithmetic — that is shared with
// EtcdDefragPolicy and tested there — but the two things unique to backups:
// that a due tick actually produces a snapshot with a distinct destination, and
// that history GC never removes a successful snapshot it was not supposed to.
// A retention bug here deletes the only artifact a restore could have used.

func snapshotPolicy(name, schedule string, opts ...func(*lll.EtcdSnapshotPolicy)) *lll.EtcdSnapshotPolicy {
	p := &lll.EtcdSnapshotPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			UID:               policyUID(name),
			CreationTimestamp: metav1.NewTime(epoch),
		},
		Spec: lll.EtcdSnapshotPolicySpec{
			ClusterRef: corev1.LocalObjectReference{Name: "c1"},
			Schedule:   lll.DefragSchedule{Cron: schedule},
			Destination: lll.SnapshotLocation{S3: &lll.S3SnapshotLocation{
				Endpoint:             "https://s3.example.com",
				Bucket:               "etcd-backups",
				Key:                  "c1/",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "s3-creds"},
			}},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ownedBySnapshotPolicy mirrors what SetControllerReference writes, so
// ownedSnapshots' IsControlledBy check keeps the object.
func ownedBySnapshotPolicy(policy string) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: lll.GroupVersion.String(),
		Kind:       "EtcdSnapshotPolicy",
		Name:       policy,
		UID:        policyUID(policy),
		Controller: &controller,
	}}
}

func stampedSnapshot(name, policy string, phase lll.EtcdSnapshotStatusPhase, finished time.Time) *lll.EtcdSnapshot {
	s := &lll.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			Labels:            map[string]string{LabelSnapshotPolicy: policy, LabelCluster: "c1"},
			OwnerReferences:   ownedBySnapshotPolicy(policy),
			CreationTimestamp: metav1.NewTime(finished),
		},
		Spec: lll.EtcdSnapshotSpec{ClusterRef: corev1.LocalObjectReference{Name: "c1"}},
		Status: lll.EtcdSnapshotStatus{
			Phase: phase,
			Conditions: []metav1.Condition{{
				Type:               lll.SnapshotReady,
				Status:             metav1.ConditionFalse,
				Reason:             "Test",
				LastTransitionTime: metav1.NewTime(finished),
			}},
		},
	}
	if phase == lll.EtcdSnapshotStatusPhaseComplete {
		s.Status.Conditions[0].Status = metav1.ConditionTrue
		s.Status.Artifact = &lll.SnapshotArtifact{
			URI:       "s3://etcd-backups/c1/" + name + ".db",
			SizeBytes: 1024,
			Checksum:  "sha256:" + name,
		}
	}
	return s
}

func snapshotPolicyReconciler(t *testing.T, now time.Time, objs ...client.Object) (*EtcdSnapshotPolicyReconciler, client.Client) {
	t.Helper()
	c, s := newTestClient(t, objs...)
	return &EtcdSnapshotPolicyReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(20), now: func() time.Time { return now }}, c
}

func listSnapshots(t *testing.T, c client.Client, policy string) []lll.EtcdSnapshot {
	t.Helper()
	var runs lll.EtcdSnapshotList
	if err := c.List(context.Background(), &runs, client.InNamespace("ns"),
		client.MatchingLabels{LabelSnapshotPolicy: policy}); err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	return runs.Items
}

func reconcileSnapshotPolicy(t *testing.T, r *EtcdSnapshotPolicyReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn(name, "ns")})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestSnapshotPolicy_StampsWhenDue(t *testing.T) {
	pol := snapshotPolicy("p", "0 * * * *")
	r, c := snapshotPolicyReconciler(t, epoch.Add(90*time.Minute), pol)

	reconcileSnapshotPolicy(t, r, "p")

	runs := listSnapshots(t, c, "p")
	if len(runs) != 1 {
		t.Fatalf("stamped %d snapshots, want 1", len(runs))
	}
	run := runs[0]
	if run.Spec.ClusterRef.Name != "c1" {
		t.Errorf("stamped clusterRef = %q, want c1", run.Spec.ClusterRef.Name)
	}
	if run.Spec.Destination.S3 == nil || run.Spec.Destination.S3.Bucket != "etcd-backups" {
		t.Errorf("destination not carried onto the stamped snapshot: %+v", run.Spec.Destination)
	}
	if run.Labels[LabelCluster] != "c1" {
		t.Errorf("missing cluster label: %v", run.Labels)
	}
	if len(run.OwnerReferences) != 1 || run.OwnerReferences[0].Name != "p" {
		t.Errorf("owner refs = %+v, want the policy", run.OwnerReferences)
	}

	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	if got.Status.LastScheduleTime == nil || !got.Status.LastScheduleTime.Time.Equal(epoch.Add(time.Hour)) {
		t.Errorf("lastScheduleTime = %v, want 01:00", got.Status.LastScheduleTime)
	}
	if len(got.Status.Active) != 1 {
		t.Errorf("status.active = %v, want the stamped snapshot", got.Status.Active)
	}
}

// The destination key is a prefix and the agent appends the snapshot's own
// name, so two ticks must produce two differently-named snapshots. If the names
// collided, each run would overwrite the last and the retention history would
// be one object deep however many successes it recorded.
func TestSnapshotPolicy_ConsecutiveTicksGetDistinctNames(t *testing.T) {
	pol := snapshotPolicy("p", "0 * * * *")
	first := (&EtcdSnapshotPolicyReconciler{}).buildSnapshot(pol, epoch.Add(time.Hour))
	second := (&EtcdSnapshotPolicyReconciler{}).buildSnapshot(pol, epoch.Add(2*time.Hour))

	if first.Name == second.Name {
		t.Fatalf("two ticks produced the same snapshot name %q; each run would overwrite the last", first.Name)
	}
	// ...and the same tick must produce the same name, so a re-reconcile
	// collides instead of double-stamping.
	again := (&EtcdSnapshotPolicyReconciler{}).buildSnapshot(pol, epoch.Add(time.Hour))
	if again.Name != first.Name {
		t.Fatalf("the same tick produced two names (%q, %q); a re-reconcile would double-stamp", first.Name, again.Name)
	}
}

func TestSnapshotPolicy_NotDueYet(t *testing.T) {
	pol := snapshotPolicy("p", "0 0 * * *")
	r, c := snapshotPolicyReconciler(t, epoch.Add(time.Hour), pol)

	res := reconcileSnapshotPolicy(t, r, "p")
	if len(listSnapshots(t, c, "p")) != 0 {
		t.Fatalf("stamped a snapshot before the first tick")
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want a positive delay to the next tick", res.RequeueAfter)
	}
}

func TestSnapshotPolicy_SuspendStopsStamping(t *testing.T) {
	suspend := true
	pol := snapshotPolicy("p", "0 * * * *", func(p *lll.EtcdSnapshotPolicy) { p.Spec.Suspend = &suspend })
	r, c := snapshotPolicyReconciler(t, epoch.Add(90*time.Minute), pol)

	reconcileSnapshotPolicy(t, r, "p")

	if n := len(listSnapshots(t, c, "p")); n != 0 {
		t.Fatalf("stamped %d snapshots while suspended", n)
	}
	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, snapshotPolicyCondition); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "Suspended" {
		t.Fatalf("condition = %+v, want Active=False/Suspended", cond)
	}
}

// Forbid is the default, and it matters more for snapshots than for defrags: a
// backup that outruns its own schedule would otherwise pile concurrent streams
// onto the same cluster.
func TestSnapshotPolicy_ForbidsConcurrentRuns(t *testing.T) {
	pol := snapshotPolicy("p", "0 * * * *")
	running := stampedSnapshot("p-old", "p", lll.EtcdSnapshotStatusPhaseStarted, epoch)
	r, c := snapshotPolicyReconciler(t, epoch.Add(90*time.Minute), pol, running)

	reconcileSnapshotPolicy(t, r, "p")

	if n := len(listSnapshots(t, c, "p")); n != 1 {
		t.Fatalf("have %d snapshots, want only the still-running one", n)
	}
	// The tick is consumed even though it was skipped, so it is not retried
	// forever.
	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	if got.Status.LastScheduleTime == nil || !got.Status.LastScheduleTime.Time.Equal(epoch.Add(time.Hour)) {
		t.Errorf("lastScheduleTime = %v, want the skipped tick consumed", got.Status.LastScheduleTime)
	}
}

func TestSnapshotPolicy_AllowConcurrentStamps(t *testing.T) {
	pol := snapshotPolicy("p", "0 * * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.ConcurrencyPolicy = lll.AllowConcurrent
	})
	running := stampedSnapshot("p-old", "p", lll.EtcdSnapshotStatusPhaseStarted, epoch)
	r, c := snapshotPolicyReconciler(t, epoch.Add(90*time.Minute), pol, running)

	reconcileSnapshotPolicy(t, r, "p")

	if n := len(listSnapshots(t, c, "p")); n != 2 {
		t.Fatalf("have %d snapshots, want the running one plus a freshly stamped one", n)
	}
}

// status.lastSuccessful* is what an alert reads to answer "is this cluster
// still being backed up". It must track the newest success, not the newest
// snapshot of any kind.
func TestSnapshotPolicy_ReportsLatestSuccess(t *testing.T) {
	pol := snapshotPolicy("p", "0 0 * * *")
	older := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))
	// A later failure must not move the success marker backwards or forwards.
	failed := stampedSnapshot("p-3", "p", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(3*time.Hour))

	r, c := snapshotPolicyReconciler(t, epoch.Add(4*time.Hour), pol, older, newer, failed)
	reconcileSnapshotPolicy(t, r, "p")

	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	if got.Status.LastSuccessfulTime == nil || !got.Status.LastSuccessfulTime.Time.Equal(epoch.Add(2*time.Hour)) {
		t.Fatalf("lastSuccessfulTime = %v, want the newer success at 02:00", got.Status.LastSuccessfulTime)
	}
	if got.Status.LastSuccessfulArtifact == nil || got.Status.LastSuccessfulArtifact.URI != "s3://etcd-backups/c1/p-2.db" {
		t.Fatalf("lastSuccessfulArtifact = %+v, want p-2's artifact", got.Status.LastSuccessfulArtifact)
	}
}

func TestSnapshotPolicy_PrunesOldestSuccessesBeyondLimit(t *testing.T) {
	limit := int32(2)
	pol := snapshotPolicy("p", "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.SuccessfulHistoryLimit = &limit
	})
	objs := []client.Object{pol}
	for i := 1; i <= 4; i++ {
		objs = append(objs, stampedSnapshot(fmt.Sprintf("p-%d", i), "p",
			lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Duration(i)*time.Hour)))
	}
	r, c := snapshotPolicyReconciler(t, epoch.Add(5*time.Hour), objs...)

	reconcileSnapshotPolicy(t, r, "p")

	kept := map[string]bool{}
	for _, s := range listSnapshots(t, c, "p") {
		if s.Status.Phase == lll.EtcdSnapshotStatusPhaseComplete {
			kept[s.Name] = true
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d successes, want 2: %v", len(kept), kept)
	}
	// The NEWEST must survive — pruning the wrong end would leave only stale
	// backups behind.
	if !kept["p-3"] || !kept["p-4"] {
		t.Fatalf("kept the wrong successes: %v", kept)
	}
}

// Successes and failures are capped separately so a burst of failures cannot
// evict the successful snapshots a restore depends on.
func TestSnapshotPolicy_FailuresDoNotEvictSuccesses(t *testing.T) {
	success, failure := int32(2), int32(1)
	pol := snapshotPolicy("p", "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.SuccessfulHistoryLimit = &success
		p.Spec.FailedHistoryLimit = &failure
	})
	objs := []client.Object{
		pol,
		stampedSnapshot("ok-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour)),
		stampedSnapshot("ok-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour)),
		stampedSnapshot("bad-1", "p", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(3*time.Hour)),
		stampedSnapshot("bad-2", "p", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(4*time.Hour)),
		stampedSnapshot("bad-3", "p", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(5*time.Hour)),
	}
	r, c := snapshotPolicyReconciler(t, epoch.Add(6*time.Hour), objs...)

	reconcileSnapshotPolicy(t, r, "p")

	var successes, failures int
	for _, s := range listSnapshots(t, c, "p") {
		switch s.Status.Phase {
		case lll.EtcdSnapshotStatusPhaseComplete:
			successes++
		case lll.EtcdSnapshotStatusPhaseFailed:
			failures++
		}
	}
	if successes != 2 {
		t.Fatalf("kept %d successes, want both: a failure burst must not evict them", successes)
	}
	if failures != 1 {
		t.Fatalf("kept %d failures, want 1", failures)
	}
}

// A snapshot that merely carries the policy label but is controlled by
// something else (or nothing) must never be counted as active nor deleted. It
// is somebody else's backup record.
func TestSnapshotPolicy_IgnoresUnownedSnapshots(t *testing.T) {
	limit := int32(0)
	pol := snapshotPolicy("p", "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.SuccessfulHistoryLimit = &limit
	})
	stray := stampedSnapshot("stray", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	stray.OwnerReferences = nil

	r, c := snapshotPolicyReconciler(t, epoch.Add(2*time.Hour), pol, stray)
	reconcileSnapshotPolicy(t, r, "p")

	found := false
	for _, s := range listSnapshots(t, c, "p") {
		if s.Name == "stray" {
			found = true
		}
	}
	if !found {
		t.Fatalf("history GC deleted a snapshot this policy does not own")
	}
	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	if got.Status.LastSuccessfulTime != nil {
		t.Fatalf("an unowned snapshot was reported as this policy's success")
	}
}

func TestSnapshotPolicy_InvalidScheduleSurfaces(t *testing.T) {
	pol := snapshotPolicy("p", "not a schedule")
	r, c := snapshotPolicyReconciler(t, epoch.Add(time.Hour), pol)

	reconcileSnapshotPolicy(t, r, "p")

	got := mustGet(t, c, "p", "ns", &lll.EtcdSnapshotPolicy{})
	cond := apimeta.FindStatusCondition(got.Status.Conditions, snapshotPolicyCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidSchedule" {
		t.Fatalf("condition = %+v, want Active=False/InvalidSchedule", cond)
	}
}

// A re-reconcile of the same tick must not stamp a second snapshot.
func TestSnapshotPolicy_TickIsStampedOnce(t *testing.T) {
	pol := snapshotPolicy("p", "0 * * * *")
	r, c := snapshotPolicyReconciler(t, epoch.Add(90*time.Minute), pol)

	reconcileSnapshotPolicy(t, r, "p")
	reconcileSnapshotPolicy(t, r, "p")

	if n := len(listSnapshots(t, c, "p")); n != 1 {
		t.Fatalf("stamped %d snapshots for one tick, want 1", n)
	}
}

func TestSnapshotHistoryLimit_Defaults(t *testing.T) {
	if got := snapshotHistoryLimit(nil, 10); got != 10 {
		t.Fatalf("unset limit = %d, want the documented default 10", got)
	}
	zero := int32(0)
	if got := snapshotHistoryLimit(&zero, 10); got != 0 {
		t.Fatalf("explicit 0 = %d, want 0 — an explicit zero is not 'unset'", got)
	}
}

// ── artifact pruning ─────────────────────────────────────────────────────
//
// The dangerous half of retention. These fix the two properties that keep a
// retention policy from being a data-loss mechanism: it deletes exactly the one
// object the pruned snapshot recorded, and a prune that fails leaves the
// EtcdSnapshot in place so the artifact stays traceable.

func pruneReconciler(t *testing.T, now time.Time, objs ...client.Object) (*EtcdSnapshotPolicyReconciler, client.Client) {
	t.Helper()
	r, c := snapshotPolicyReconciler(t, now, objs...)
	r.OperatorImage = "ghcr.io/cozystack/etcd-operator:test"
	return r, c
}

func getJob(t *testing.T, c client.Client, name string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), nn(name, "ns"), job); err != nil {
		return nil
	}
	return job
}

func prunePolicy(name string) *lll.EtcdSnapshotPolicy {
	limit := int32(1)
	return snapshotPolicy(name, "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.SuccessfulHistoryLimit = &limit
		p.Spec.DeleteArtifactOnPrune = true
	})
}

// The first pass over an over-limit snapshot starts a prune Job and keeps the
// object: its status.artifact is the only record of where the stored snapshot
// lives, so deleting it first would orphan the object on any failure.
func TestSnapshotPolicy_PruneStartsJobAndKeepsSnapshot(t *testing.T) {
	pol := prunePolicy("p")
	old := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))
	r, c := pruneReconciler(t, epoch.Add(3*time.Hour), pol, old, newer)

	reconcileSnapshotPolicy(t, r, "p")

	job := getJob(t, c, pruneJobName("p-1"))
	if job == nil {
		t.Fatalf("no prune Job created for the over-limit snapshot")
	}
	if len(job.Spec.Template.Spec.Containers) != 1 ||
		job.Spec.Template.Spec.Containers[0].Command[1] != "prune-agent" {
		t.Fatalf("prune Job does not run the prune agent: %+v", job.Spec.Template.Spec.Containers)
	}
	// The Job must name the one snapshot it may delete.
	var named bool
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SNAPSHOT_NAME" && e.Value == "p-1" {
			named = true
		}
	}
	if !named {
		t.Fatalf("prune Job does not name the snapshot to delete: %+v", job.Spec.Template.Spec.Containers[0].Env)
	}
	// Owned by the policy — a Job owned by the snapshot it prunes would be
	// garbage-collected the moment that snapshot is deleted.
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "EtcdSnapshotPolicy" {
		t.Fatalf("prune Job owner = %+v, want the policy", job.OwnerReferences)
	}

	if err := c.Get(context.Background(), nn("p-1", "ns"), &lll.EtcdSnapshot{}); err != nil {
		t.Fatalf("EtcdSnapshot deleted before its artifact was pruned: %v", err)
	}
}

func TestSnapshotPolicy_PruneDeletesSnapshotOnceJobSucceeds(t *testing.T) {
	pol := prunePolicy("p")
	old := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))
	r, c := pruneReconciler(t, epoch.Add(3*time.Hour), pol, old, newer)

	reconcileSnapshotPolicy(t, r, "p")

	job := getJob(t, c, pruneJobName("p-1"))
	if job == nil {
		t.Fatalf("no prune Job created")
	}
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("mark job succeeded: %v", err)
	}

	reconcileSnapshotPolicy(t, r, "p")

	if err := c.Get(context.Background(), nn("p-1", "ns"), &lll.EtcdSnapshot{}); err == nil {
		t.Fatalf("EtcdSnapshot survived a successful artifact prune")
	}
	if err := c.Get(context.Background(), nn("p-2", "ns"), &lll.EtcdSnapshot{}); err != nil {
		t.Fatalf("the within-limit snapshot was deleted too: %v", err)
	}
}

// A prune that fails must not take the EtcdSnapshot with it: the object is the
// only thing that records where the stored snapshot is.
func TestSnapshotPolicy_FailedPruneKeepsSnapshot(t *testing.T) {
	pol := prunePolicy("p")
	old := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))
	r, c := pruneReconciler(t, epoch.Add(3*time.Hour), pol, old, newer)

	reconcileSnapshotPolicy(t, r, "p")

	job := getJob(t, c, pruneJobName("p-1"))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("mark job failed: %v", err)
	}

	reconcileSnapshotPolicy(t, r, "p")

	if err := c.Get(context.Background(), nn("p-1", "ns"), &lll.EtcdSnapshot{}); err != nil {
		t.Fatalf("EtcdSnapshot deleted despite a failed artifact prune: %v", err)
	}
}

// Without DeleteArtifactOnPrune the object is pruned and the stored snapshot is
// left alone — no Job, no deletion.
func TestSnapshotPolicy_ArtifactKeptWhenPruningDisabled(t *testing.T) {
	limit := int32(1)
	pol := snapshotPolicy("p", "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.SuccessfulHistoryLimit = &limit
	})
	old := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))
	r, c := pruneReconciler(t, epoch.Add(3*time.Hour), pol, old, newer)

	reconcileSnapshotPolicy(t, r, "p")

	if job := getJob(t, c, pruneJobName("p-1")); job != nil {
		t.Fatalf("prune Job created with deleteArtifactOnPrune off")
	}
	if err := c.Get(context.Background(), nn("p-1", "ns"), &lll.EtcdSnapshot{}); err == nil {
		t.Fatalf("the EtcdSnapshot should still be pruned; only the artifact is kept")
	}
}

// A failed snapshot never produced an artifact, so pruning it is a plain object
// delete even with artifact pruning on. Starting a Job for it would fail
// against a key that was never written.
func TestSnapshotPolicy_FailedSnapshotsPruneWithoutAJob(t *testing.T) {
	limit := int32(0)
	pol := snapshotPolicy("p", "0 0 * * *", func(p *lll.EtcdSnapshotPolicy) {
		p.Spec.FailedHistoryLimit = &limit
		p.Spec.DeleteArtifactOnPrune = true
	})
	bad := stampedSnapshot("bad-1", "p", lll.EtcdSnapshotStatusPhaseFailed, epoch.Add(time.Hour))
	r, c := pruneReconciler(t, epoch.Add(2*time.Hour), pol, bad)

	reconcileSnapshotPolicy(t, r, "p")

	if job := getJob(t, c, pruneJobName("bad-1")); job != nil {
		t.Fatalf("prune Job created for a snapshot that never wrote an artifact")
	}
	if err := c.Get(context.Background(), nn("bad-1", "ns"), &lll.EtcdSnapshot{}); err == nil {
		t.Fatalf("failed snapshot was not pruned")
	}
}

// With no operator image there is no agent to run. Retaining silently is how a
// destination fills up unnoticed, so the snapshot is kept AND reported.
func TestSnapshotPolicy_PruneWithoutOperatorImageIsReported(t *testing.T) {
	pol := prunePolicy("p")
	old := stampedSnapshot("p-1", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(time.Hour))
	newer := stampedSnapshot("p-2", "p", lll.EtcdSnapshotStatusPhaseComplete, epoch.Add(2*time.Hour))

	r, c := snapshotPolicyReconciler(t, epoch.Add(3*time.Hour), pol, old, newer) // no OperatorImage
	rec := r.Recorder.(*record.FakeRecorder)

	reconcileSnapshotPolicy(t, r, "p")

	if err := c.Get(context.Background(), nn("p-1", "ns"), &lll.EtcdSnapshot{}); err != nil {
		t.Fatalf("snapshot pruned even though its artifact could not be: %v", err)
	}
	var reported bool
	for len(rec.Events) > 0 {
		if strings.Contains(<-rec.Events, "PruneUnavailable") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("no PruneUnavailable event; the policy would look like it prunes and silently not")
	}
}
