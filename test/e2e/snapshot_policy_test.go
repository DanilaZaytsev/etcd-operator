//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha2 "github.com/cozystack/etcd-operator/api/v1alpha2"
)

const (
	policyNamespace = "snapshot-policy-e2e"
	policyCluster   = "etcd"
	policyName      = "every-minute"
	policyClaim     = "snapshots"
)

// TestSnapshotPolicyRunsAndPrunes proves the scheduled-backup loop closes on a
// live cluster: the policy fires on its cron, the runs actually store snapshots,
// and history GC keeps the object count at the configured limit instead of
// growing without bound.
//
// The pruning half matters more than it looks. Successes and failures are capped
// separately on purpose — a run of failures must not evict the successful
// snapshots, because those are the only things a restore can point at. A single
// shared limit would silently turn a bad week into "no recoverable backups", and
// nothing about the CR surface would show it.
func TestSnapshotPolicyRunsAndPrunes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	createNamespace(ctx, t, policyNamespace)

	claim := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: policyClaim, Namespace: policyNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("512Mi")},
			},
		},
	}
	if err := kube.Create(ctx, claim); err != nil {
		t.Fatalf("create snapshot PVC: %v", err)
	}

	one := int32(1)
	ec := &etcdv1alpha2.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: policyCluster, Namespace: policyNamespace},
		Spec: etcdv1alpha2.EtcdClusterSpec{
			Replicas: &one,
			Version:  "3.6.11",
			Storage:  etcdv1alpha2.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
	if err := kube.Create(ctx, ec); err != nil {
		t.Fatalf("create EtcdCluster: %v", err)
	}
	waitFor(ctx, t, 5*time.Minute, "cluster Available", etcdClusterAvailable(policyNamespace, policyCluster))
	waitFor(ctx, t, 2*time.Minute, "1 member ready", readyMembersIsIn(policyNamespace, policyCluster, 1))

	// Every minute, keeping one success. Two ticks are enough to observe both
	// halves: the second run proves the schedule repeats, and the limit of 1
	// proves the first run's object was pruned rather than accumulated.
	keepOne := int32(1)
	pol := &etcdv1alpha2.EtcdSnapshotPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: policyNamespace},
		Spec: etcdv1alpha2.EtcdSnapshotPolicySpec{
			ClusterRef: corev1.LocalObjectReference{Name: policyCluster},
			Schedule:   etcdv1alpha2.DefragSchedule{Cron: "* * * * *"},
			Destination: etcdv1alpha2.SnapshotLocation{
				PVC: &etcdv1alpha2.PVCSnapshotLocation{ClaimName: policyClaim, SubPath: "scheduled"},
			},
			SuccessfulHistoryLimit: &keepOne,
		},
	}
	if err := kube.Create(ctx, pol); err != nil {
		t.Fatalf("create EtcdSnapshotPolicy: %v", err)
	}

	// First success: the policy has to reach the cluster, run a Job, store an
	// artifact and stamp its own status. A schedule that fires but never
	// records a success is indistinguishable from no backups at all.
	waitFor(ctx, t, 6*time.Minute, "policy records a successful run", func(ctx context.Context) error {
		got := &etcdv1alpha2.EtcdSnapshotPolicy{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: policyNamespace, Name: policyName}, got); err != nil {
			return err
		}
		if got.Status.LastScheduleTime == nil {
			return fmt.Errorf("no lastScheduleTime yet")
		}
		if got.Status.LastSuccessfulTime == nil {
			return fmt.Errorf("scheduled at %s but no successful run yet", got.Status.LastScheduleTime.Time)
		}
		if got.Status.LastSuccessfulArtifact == nil || got.Status.LastSuccessfulArtifact.Checksum == "" {
			return fmt.Errorf("successful run recorded without an artifact checksum — nothing a restore could verify")
		}
		return nil
	})

	firstSuccess := policySuccessTime(ctx, t)
	t.Logf("first scheduled snapshot stored at %s", firstSuccess)

	// Second tick, then the pruning assertion. Waiting for the success time to
	// move is what distinguishes "the schedule repeats" from "one run happened
	// and the controller went quiet".
	waitFor(ctx, t, 6*time.Minute, "a second scheduled run completes", func(ctx context.Context) error {
		if got := policySuccessTimeOrZero(ctx); !got.IsZero() && got.After(firstSuccess) {
			return nil
		}
		return fmt.Errorf("still on the first run")
	})

	waitFor(ctx, t, 3*time.Minute, "history pruned to successfulHistoryLimit", func(ctx context.Context) error {
		list := &etcdv1alpha2.EtcdSnapshotList{}
		if err := kube.List(ctx, list, client.InNamespace(policyNamespace),
			client.MatchingLabels{"etcd-operator.cozystack.io/snapshot-policy": policyName}); err != nil {
			return err
		}
		complete := 0
		for i := range list.Items {
			if list.Items[i].Status.Phase == etcdv1alpha2.EtcdSnapshotStatusPhaseComplete {
				complete++
			}
		}
		if complete > int(keepOne) {
			return fmt.Errorf("%d Complete snapshots retained, want at most %d", complete, keepOne)
		}
		if complete == 0 {
			// Pruning to zero would be worse than not pruning: the limit is a
			// cap on retained successes, not a licence to delete the last one.
			return fmt.Errorf("no Complete snapshots retained at all")
		}
		return nil
	})
	t.Logf("history held at successfulHistoryLimit=%d across repeated runs", keepOne)
}

func policySuccessTime(ctx context.Context, t *testing.T) time.Time {
	t.Helper()
	got := &etcdv1alpha2.EtcdSnapshotPolicy{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: policyNamespace, Name: policyName}, got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if got.Status.LastSuccessfulTime == nil {
		t.Fatalf("policy has no lastSuccessfulTime")
	}
	return got.Status.LastSuccessfulTime.Time
}

func policySuccessTimeOrZero(ctx context.Context) time.Time {
	got := &etcdv1alpha2.EtcdSnapshotPolicy{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: policyNamespace, Name: policyName}, got); err != nil {
		return time.Time{}
	}
	if got.Status.LastSuccessfulTime == nil {
		return time.Time{}
	}
	return got.Status.LastSuccessfulTime.Time
}
