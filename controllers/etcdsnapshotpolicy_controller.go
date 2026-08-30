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
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

const (
	// snapshotPolicyCondition is the single condition type on an
	// EtcdSnapshotPolicy: True while the policy is actively scheduling, False
	// (with the reason) when suspended or holding an unparseable schedule.
	snapshotPolicyCondition = "Active"

	// snapshotPolicyMaxCatchup bounds how many missed ticks nextSchedule walks
	// to find the most recent one. Same role as its defrag counterpart: never
	// replays slots, just caps the walk so a clock jump surfaces as a condition
	// rather than an unbounded loop.
	snapshotPolicyMaxCatchup = 100
)

// EtcdSnapshotPolicyReconciler stamps out EtcdSnapshot runs on a cron schedule
// so the operator drives recurring backups itself, rather than leaving the
// cadence to an external CronJob that may or may not exist. Each run is a
// discrete EtcdSnapshot owned by the policy (so it cascades on delete) and
// labelled with the policy name (so the controller can find its own runs).
//
// The scheduling machinery — parseSchedule, nextSchedule, lookbackFloor,
// requeueFor — is shared with EtcdDefragPolicy rather than reimplemented: the
// catch-up and starting-deadline semantics are subtle enough that two copies
// would drift.
type EtcdSnapshotPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits scheduling events. Tests may leave it nil.
	Recorder record.EventRecorder

	// OperatorImage is the operator's own image, which the prune agent runs
	// from. Empty means artifact pruning is unavailable; a policy that asks for
	// it is surfaced on the condition rather than silently retaining forever.
	OperatorImage string

	// now is the clock, overridable in tests. nil means time.Now.
	now func() time.Time
}

//+kubebuilder:rbac:groups=etcd-operator.cozystack.io,resources=etcdsnapshotpolicies,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=etcd-operator.cozystack.io,resources=etcdsnapshotpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=etcd-operator.cozystack.io,resources=etcdsnapshots,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *EtcdSnapshotPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pol := &lll.EtcdSnapshotPolicy{}
	if err := r.Get(ctx, req.NamespacedName, pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Observe the snapshots this policy owns. The label narrows the list
	// server-side; the ownerRef is the authority, so a hand-copied snapshot that
	// kept the label but isn't controlled by this policy is neither counted as
	// active nor deleted by history GC. Deleting someone else's backup record
	// because it carried a matching label would be the worst bug this
	// controller could have.
	var runs lll.EtcdSnapshotList
	if err := r.List(ctx, &runs, client.InNamespace(pol.Namespace),
		client.MatchingLabels{LabelSnapshotPolicy: pol.Name}); err != nil {
		return ctrl.Result{}, err
	}
	owned := ownedSnapshots(runs.Items, pol)
	active, succeeded, failed := partitionSnapshotRuns(owned)
	pol.Status.Active = snapshotRunRefs(active)
	if latest := latestCompleteSnapshot(succeeded); latest != nil {
		t := metav1.NewTime(snapshotFinishTime(latest))
		pol.Status.LastSuccessfulTime = &t
		pol.Status.LastSuccessfulArtifact = latest.Status.Artifact.DeepCopy()
	}

	// Trim history. Successes and failures are capped separately (as CronJob
	// does): a burst of failures must not evict the successful snapshots that
	// are the only thing a restore can use.
	if err := r.gcHistory(ctx, pol, succeeded, snapshotHistoryLimit(pol.Spec.SuccessfulHistoryLimit, 10)); err != nil {
		return ctrl.Result{}, err
	}
	// Failed snapshots never produced an artifact, so they are always a plain
	// object delete.
	if err := r.gcHistory(ctx, pol, failed, snapshotHistoryLimit(pol.Spec.FailedHistoryLimit, 3)); err != nil {
		return ctrl.Result{}, err
	}

	if pol.Spec.Suspend != nil && *pol.Spec.Suspend {
		setSnapshotPolicyCondition(pol, metav1.ConditionFalse, "Suspended", "scheduling is suspended")
		return ctrl.Result{}, r.Status().Update(ctx, pol)
	}

	sched, err := parseSchedule(pol.Spec.Schedule)
	if err != nil {
		setSnapshotPolicyCondition(pol, metav1.ConditionFalse, "InvalidSchedule",
			fmt.Sprintf("cannot parse schedule %q: %v", pol.Spec.Schedule.Cron, err))
		// Only a spec change can fix this; the watch re-triggers, so don't requeue.
		return ctrl.Result{}, r.Status().Update(ctx, pol)
	}
	setSnapshotPolicyCondition(pol, metav1.ConditionTrue, "Scheduled", "policy is scheduling snapshots")

	now := r.clock()
	earliest := pol.CreationTimestamp.Time
	if pol.Status.LastScheduleTime != nil {
		earliest = pol.Status.LastScheduleTime.Time
	}
	anchor := earliest
	cutoff := now.Add(-lookbackFloor(sched, now))
	if d := pol.Spec.StartingDeadlineSeconds; d != nil {
		cutoff = now.Add(-time.Duration(*d) * time.Second)
	}
	if cutoff.After(earliest) {
		earliest = cutoff
	}
	// A backup that was not taken is exactly the thing nobody notices until a
	// restore. Say so rather than letting the policy report Scheduled with no
	// trace of the skip.
	if dropped := sched.Next(anchor); earliest.After(anchor) && dropped.Before(earliest) {
		why := "further behind than one schedule period"
		if pol.Spec.StartingDeadlineSeconds != nil {
			why = fmt.Sprintf("past the %ds starting deadline", *pol.Spec.StartingDeadlineSeconds)
		}
		r.event(pol, corev1.EventTypeWarning, "MissedSchedule",
			fmt.Sprintf("skipped scheduled snapshot %s: %s late, %s",
				tickString(dropped), now.Sub(dropped).Truncate(time.Second), why))
	}
	due, next, err := nextSchedule(sched, earliest, now, snapshotPolicyMaxCatchup)
	if err != nil {
		setSnapshotPolicyCondition(pol, metav1.ConditionFalse, "TooManyMissedTicks", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, pol)
	}

	if due == nil {
		if err := r.Status().Update(ctx, pol); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueFor(next, now)}, nil
	}
	tick := *due

	if snapshotConcurrencyPolicy(pol) == lll.ForbidConcurrent && len(active) > 0 {
		r.event(pol, corev1.EventTypeNormal, "ConcurrencyForbidden",
			fmt.Sprintf("skipped scheduled snapshot %s: %d snapshot(s) still running", tickString(tick), len(active)))
		pol.Status.LastScheduleTime = &metav1.Time{Time: tick}
		if err := r.Status().Update(ctx, pol); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueFor(next, now)}, nil
	}

	run := r.buildSnapshot(pol, tick)
	if err := controllerutil.SetControllerReference(pol, run, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, run); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// A rejected Create (e.g. a destination the stamped snapshot's CEL
			// rejects) otherwise leaves the policy looking healthy while it
			// silently never backs anything up.
			setSnapshotPolicyCondition(pol, metav1.ConditionFalse, "StampFailed",
				fmt.Sprintf("cannot stamp EtcdSnapshot for %s: %v", tickString(tick), err))
			if uerr := r.Status().Update(ctx, pol); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{}, err
		}
		// The deterministic name means a re-reconcile of the same tick is a
		// no-op rather than a duplicate snapshot.
		logger.Info("snapshot already stamped for this tick", "tick", tickString(tick), "name", run.Name)
	} else {
		r.event(pol, corev1.EventTypeNormal, "StampedSnapshot",
			fmt.Sprintf("stamped EtcdSnapshot %q for scheduled time %s", run.Name, tickString(tick)))
		pol.Status.Active = append(pol.Status.Active, corev1.LocalObjectReference{Name: run.Name})
	}
	pol.Status.LastScheduleTime = &metav1.Time{Time: tick}
	if err := r.Status().Update(ctx, pol); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueFor(next, now)}, nil
}

// buildSnapshot renders the EtcdSnapshot stamped for a tick. The name is
// deterministic in the scheduled time so a re-reconcile of the same tick
// collides (IsAlreadyExists) instead of double-stamping, and — because the
// snapshot agent appends the snapshot's own name to the destination key — it is
// also what keeps successive runs from overwriting each other's artifact.
func (r *EtcdSnapshotPolicyReconciler) buildSnapshot(pol *lll.EtcdSnapshotPolicy, tick time.Time) *lll.EtcdSnapshot {
	return &lll.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", pol.Name, tick.Unix()/60),
			Namespace: pol.Namespace,
			Labels: map[string]string{
				LabelSnapshotPolicy: pol.Name,
				LabelCluster:        pol.Spec.ClusterRef.Name,
			},
		},
		Spec: lll.EtcdSnapshotSpec{
			ClusterRef:  pol.Spec.ClusterRef,
			Destination: *pol.Spec.Destination.DeepCopy(),
		},
	}
}

// gcHistory deletes the oldest finished snapshots beyond limit.
//
// With DeleteArtifactOnPrune the stored snapshot goes first, via a prune Job,
// and the EtcdSnapshot is deleted only once that Job has succeeded. The order
// matters: the EtcdSnapshot's status.artifact is the only record of where the
// object lives, so deleting the object first would, on a failure, leave an
// orphan nobody can find. A snapshot whose prune fails is kept and reported —
// retaining one object past its limit is the better failure.
func (r *EtcdSnapshotPolicyReconciler) gcHistory(
	ctx context.Context,
	pol *lll.EtcdSnapshotPolicy,
	finished []lll.EtcdSnapshot,
	limit int,
) error {
	if len(finished) <= limit {
		return nil
	}
	sort.Slice(finished, func(i, j int) bool {
		return snapshotFinishTime(&finished[i]).Before(snapshotFinishTime(&finished[j]))
	})
	for i := 0; i < len(finished)-limit; i++ {
		snapshot := &finished[i]

		if pol.Spec.DeleteArtifactOnPrune && snapshot.Status.Artifact != nil {
			done, err := r.ensureArtifactPruned(ctx, pol, snapshot)
			if err != nil {
				return err
			}
			if !done {
				// Prune still running, or failed and reported. Keep the object:
				// it carries the only pointer to the artifact.
				continue
			}
		}
		if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ensureArtifactPruned drives one snapshot's prune Job to completion, reporting
// whether the stored artifact is now gone. It is idempotent across reconciles:
// the Job name is derived from the snapshot's, so a repeat finds its own Job
// instead of starting a second one.
func (r *EtcdSnapshotPolicyReconciler) ensureArtifactPruned(
	ctx context.Context,
	pol *lll.EtcdSnapshotPolicy,
	snapshot *lll.EtcdSnapshot,
) (bool, error) {
	if r.OperatorImage == "" {
		// Without an image there is no agent to run. Say so rather than
		// retaining silently — a policy that looks like it prunes and does not
		// is how a destination fills up unnoticed.
		r.event(pol, corev1.EventTypeWarning, "PruneUnavailable",
			fmt.Sprintf("cannot prune the artifact of %q: the operator image is not configured "+
				"(set --operator-image / OPERATOR_IMAGE)", snapshot.Name))
		return false, nil
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: pruneJobName(snapshot.Name)}, job)
	switch {
	case apierrors.IsNotFound(err):
		job = buildPruneJob(snapshot, r.OperatorImage)
		// Owned by the policy, not by the snapshot it prunes: a Job owned by the
		// object about to be deleted would be garbage-collected mid-run.
		if err := controllerutil.SetControllerReference(pol, job, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		r.event(pol, corev1.EventTypeNormal, "PruningArtifact",
			fmt.Sprintf("deleting the stored snapshot for %q (%s)", snapshot.Name, snapshot.Status.Artifact.URI))
		return false, nil
	case err != nil:
		return false, err
	}

	switch {
	case job.Status.Succeeded > 0:
		return true, nil
	case jobFailed(job):
		r.event(pol, corev1.EventTypeWarning, "PruneFailed",
			fmt.Sprintf("could not delete the stored snapshot for %q (%s); keeping the EtcdSnapshot so the artifact stays traceable",
				snapshot.Name, snapshot.Status.Artifact.URI))
		return false, nil
	default:
		return false, nil
	}
}

func (r *EtcdSnapshotPolicyReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *EtcdSnapshotPolicyReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

func (r *EtcdSnapshotPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.now == nil {
		r.now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&lll.EtcdSnapshotPolicy{}).
		Owns(&lll.EtcdSnapshot{}).
		// Prune Jobs are policy-owned; watching them means a finished prune
		// re-triggers the reconcile that deletes the pruned EtcdSnapshot,
		// instead of that waiting for the next scheduled tick.
		Owns(&batchv1.Job{}).
		Complete(r)
}

// ── pure helpers ────────────────────────────────────────────────────────────

func snapshotConcurrencyPolicy(pol *lll.EtcdSnapshotPolicy) lll.ConcurrencyPolicy {
	if pol.Spec.ConcurrencyPolicy == "" {
		return lll.ForbidConcurrent
	}
	return pol.Spec.ConcurrencyPolicy
}

// snapshotHistoryLimit resolves an unset limit to the field's documented
// default. The CRD default covers objects created through the apiserver; this
// covers a policy built in-process (tests, and any future caller).
func snapshotHistoryLimit(p *int32, fallback int) int {
	if p == nil {
		return fallback
	}
	return int(*p)
}

// ownedSnapshots keeps only the EtcdSnapshots this policy actually controls (by
// ownerRef), dropping any that merely carry the policy label.
func ownedSnapshots(items []lll.EtcdSnapshot, pol *lll.EtcdSnapshotPolicy) []lll.EtcdSnapshot {
	owned := make([]lll.EtcdSnapshot, 0, len(items))
	for i := range items {
		if metav1.IsControlledBy(&items[i], pol) {
			owned = append(owned, items[i])
		}
	}
	return owned
}

// partitionSnapshotRuns splits owned snapshots into still-running, Complete and
// Failed. Successes and failures are returned separately because they are
// retained under separate limits.
func partitionSnapshotRuns(items []lll.EtcdSnapshot) (active, succeeded, failed []lll.EtcdSnapshot) {
	for i := range items {
		switch items[i].Status.Phase {
		case lll.EtcdSnapshotStatusPhaseComplete:
			succeeded = append(succeeded, items[i])
		case lll.EtcdSnapshotStatusPhaseFailed:
			failed = append(failed, items[i])
		default:
			active = append(active, items[i])
		}
	}
	return active, succeeded, failed
}

func snapshotRunRefs(items []lll.EtcdSnapshot) []corev1.LocalObjectReference {
	if len(items) == 0 {
		return nil
	}
	out := make([]corev1.LocalObjectReference, 0, len(items))
	for i := range items {
		out = append(out, corev1.LocalObjectReference{Name: items[i].Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// latestCompleteSnapshot returns the most recently finished Complete snapshot,
// which is what status.lastSuccessful{Time,Artifact} report.
func latestCompleteSnapshot(succeeded []lll.EtcdSnapshot) *lll.EtcdSnapshot {
	var best *lll.EtcdSnapshot
	for i := range succeeded {
		s := &succeeded[i]
		if best == nil || snapshotFinishTime(s).After(snapshotFinishTime(best)) {
			best = s
		}
	}
	return best
}

// snapshotFinishTime orders finished snapshots for history GC and for picking
// the latest success. EtcdSnapshot records no completion timestamp of its own,
// so the Ready condition's last transition — the moment the phase became
// terminal — is the finish time, falling back to creation time.
func snapshotFinishTime(s *lll.EtcdSnapshot) time.Time {
	for i := range s.Status.Conditions {
		if s.Status.Conditions[i].Type == lll.SnapshotReady {
			if t := s.Status.Conditions[i].LastTransitionTime; !t.IsZero() {
				return t.Time
			}
		}
	}
	return s.CreationTimestamp.Time
}

func setSnapshotPolicyCondition(pol *lll.EtcdSnapshotPolicy, status metav1.ConditionStatus, reason, msg string) {
	setCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               snapshotPolicyCondition,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pol.Generation,
	})
}
