//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha2 "github.com/cozystack/etcd-operator/api/v1alpha2"
)

const (
	restoreNamespace  = "restore-e2e"
	restoreCluster    = "etcd"
	restoreBadCluster = "badsum"
	restoreClaim      = "snapshots"
	restoreDestDir    = "backups"
	restoreKey        = "/e2e/pre-disaster-sentinel"
	restoreValue      = "written-before-the-disaster"
)

// TestRestoreFromSnapshotWithChecksum walks the disaster-recovery runbook end to
// end against a live cluster: write, snapshot, destroy the cluster together with
// its volumes, rebuild from the snapshot, read the value back.
//
// Everything below the CR surface is already covered by unit tests — the agent's
// download, the init-container wiring, the checksum comparison. What none of
// them answer is whether the data actually comes back, because they all mock the
// path rather than travel it. The two halves of the round trip are also written
// by different code with different meanings for the same field: a destination
// subPath is a *directory* the agent appends a filename to, while a restore
// source subPath is the *exact file*. A test that hardcodes one of them would
// pass while the runbook it stands for fails, so the restore path here is
// derived from the artifact URI the operator published — exactly what an
// operator following the runbook reads off `status.artifact`.
//
// The second half asserts the negative: a snapshot whose checksum does not match
// must be refused. etcd's own snapshot carries no hash, so `etcdutl snapshot
// restore` runs with --skip-hash-check and this comparison is the only integrity
// check on the whole path. If it silently passed, a truncated object would
// produce a data dir that etcd starts happily on, and the cluster would look
// healthy while serving a fraction of its keyspace.
func TestRestoreFromSnapshotWithChecksum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	createNamespace(ctx, t, restoreNamespace)

	// The snapshot lands on a PVC rather than S3: no object store to stand up,
	// and the restore path is identical past the download.
	claim := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: restoreClaim, Namespace: restoreNamespace},
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
		ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: restoreNamespace},
		Spec: etcdv1alpha2.EtcdClusterSpec{
			Replicas: &one,
			Version:  "3.6.11",
			Storage:  etcdv1alpha2.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
	if err := kube.Create(ctx, ec); err != nil {
		t.Fatalf("create EtcdCluster: %v", err)
	}
	waitFor(ctx, t, 5*time.Minute, "cluster Available", etcdClusterAvailable(restoreNamespace, restoreCluster))
	waitFor(ctx, t, 2*time.Minute, "1 member ready", readyMembersIsIn(restoreNamespace, restoreCluster, 1))

	// The value that has to survive. Written before anything is snapshotted, so
	// finding it afterwards can only mean it travelled through the snapshot.
	writeSentinel(ctx, t, restoreNamespace, restoreCluster, restoreKey, restoreValue)

	snapName := "dr"
	snap := &etcdv1alpha2.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: restoreNamespace},
		Spec: etcdv1alpha2.EtcdSnapshotSpec{
			ClusterRef: corev1.LocalObjectReference{Name: restoreCluster},
			Destination: etcdv1alpha2.SnapshotLocation{
				PVC: &etcdv1alpha2.PVCSnapshotLocation{ClaimName: restoreClaim, SubPath: restoreDestDir},
			},
		},
	}
	if err := kube.Create(ctx, snap); err != nil {
		t.Fatalf("create EtcdSnapshot: %v", err)
	}

	var artifactURI, checksum string
	waitFor(ctx, t, 5*time.Minute, "snapshot stored with a checksum", func(ctx context.Context) error {
		got := &etcdv1alpha2.EtcdSnapshot{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: restoreNamespace, Name: snapName}, got); err != nil {
			return err
		}
		if got.Status.Phase == etcdv1alpha2.EtcdSnapshotStatusPhaseFailed {
			t.Fatalf("snapshot failed: %+v", got.Status)
		}
		if got.Status.Phase != etcdv1alpha2.EtcdSnapshotStatusPhaseComplete {
			return fmt.Errorf("phase=%q", got.Status.Phase)
		}
		if got.Status.Artifact == nil || got.Status.Artifact.Checksum == "" {
			// Without a published checksum there is nothing for a restore to
			// verify against, which makes the whole integrity story moot.
			return fmt.Errorf("phase Complete but status.artifact.checksum is empty")
		}
		artifactURI, checksum = got.Status.Artifact.URI, got.Status.Artifact.Checksum
		return nil
	})
	t.Logf("artifact uri=%s checksum=%s", artifactURI, checksum)

	restoreSubPath := restoreSubPathFromURI(t, artifactURI)
	t.Logf("restore source subPath=%q (the exact file, not the directory the destination named)", restoreSubPath)

	// The disaster: cluster and its data volumes both go. Deleting only the
	// cluster would leave the member PVC behind and the rebuild could come up
	// on the old data, proving nothing about the snapshot.
	destroyCluster(ctx, t, restoreNamespace, restoreCluster)

	rebuilt := &etcdv1alpha2.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: restoreCluster, Namespace: restoreNamespace},
		Spec: etcdv1alpha2.EtcdClusterSpec{
			Replicas: &one,
			Version:  "3.6.11",
			Storage:  etcdv1alpha2.StorageSpec{Size: resource.MustParse("1Gi")},
			Bootstrap: &etcdv1alpha2.BootstrapSpec{
				Restore: &etcdv1alpha2.RestoreSpec{
					Checksum: checksum,
					Source: etcdv1alpha2.SnapshotLocation{
						PVC: &etcdv1alpha2.PVCSnapshotLocation{ClaimName: restoreClaim, SubPath: restoreSubPath},
					},
				},
			},
		},
	}
	if err := kube.Create(ctx, rebuilt); err != nil {
		t.Fatalf("create restored EtcdCluster: %v", err)
	}
	waitFor(ctx, t, 10*time.Minute, "restored cluster Available", etcdClusterAvailable(restoreNamespace, restoreCluster))
	waitFor(ctx, t, 2*time.Minute, "restored member ready", readyMembersIsIn(restoreNamespace, restoreCluster, 1))

	got := readSentinel(ctx, t, restoreNamespace, restoreCluster, restoreKey)
	if got != restoreValue {
		t.Fatalf("restored cluster does not serve the pre-disaster value: got %q, want %q. "+
			"The cluster came up healthy, so the restore path ran without erroring while "+
			"producing a data dir that is not the snapshot's", got, restoreValue)
	}
	t.Logf("pre-disaster value survived the round trip: %s=%s", restoreKey, got)

	// Negative half. Same snapshot, deliberately wrong checksum: the restore
	// must refuse rather than rebuild from an object it cannot vouch for.
	bad := &etcdv1alpha2.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: restoreBadCluster, Namespace: restoreNamespace},
		Spec: etcdv1alpha2.EtcdClusterSpec{
			Replicas: &one,
			Version:  "3.6.11",
			Storage:  etcdv1alpha2.StorageSpec{Size: resource.MustParse("1Gi")},
			Bootstrap: &etcdv1alpha2.BootstrapSpec{
				Restore: &etcdv1alpha2.RestoreSpec{
					Checksum: "sha256:" + strings.Repeat("0", 64),
					Source: etcdv1alpha2.SnapshotLocation{
						PVC: &etcdv1alpha2.PVCSnapshotLocation{ClaimName: restoreClaim, SubPath: restoreSubPath},
					},
				},
			},
		},
	}
	if err := kube.Create(ctx, bad); err != nil {
		t.Fatalf("create mismatched-checksum EtcdCluster: %v", err)
	}

	// Assert on the init container's own report rather than on a timeout: a
	// "never became ready" wait would also pass if the image failed to pull.
	waitFor(ctx, t, 5*time.Minute, "restore refused on checksum mismatch", func(ctx context.Context) error {
		pods := &corev1.PodList{}
		if err := kube.List(ctx, pods, client.InNamespace(restoreNamespace),
			client.MatchingLabels{
				"etcd-operator.cozystack.io/cluster": restoreBadCluster,
				"app.kubernetes.io/name":             "etcd",
			}); err != nil {
			return err
		}
		if len(pods.Items) == 0 {
			return fmt.Errorf("no member pod yet")
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase == corev1.PodRunning {
				t.Fatalf("member %q of %q is Running: the restore accepted a snapshot whose "+
					"checksum did not match. etcdutl runs with --skip-hash-check, so this "+
					"comparison is the only integrity check on the path — a truncated object "+
					"would now be serving as the cluster's data dir", pod.Name, restoreBadCluster)
			}
			logs, _ := podLogs(ctx, restoreNamespace, pod.Name, "restore")
			if strings.Contains(logs, "checksum mismatch") {
				t.Logf("restore refused, as it must: %s", firstLine(logs))
				return nil
			}
		}
		return fmt.Errorf("member pod present but the restore container has not reported a mismatch yet")
	})
}

// restoreSubPathFromURI turns the artifact URI the operator published into the
// value a restore source needs.
//
// The two ends of the same field disagree on purpose: as a destination SubPath
// names a directory and the agent appends its own filename, as a restore source
// it names the file. Slicing the URI from the destination directory onwards
// converts one to the other without depending on the volume's mount path, which
// is an operator implementation detail this test has no business pinning.
func restoreSubPathFromURI(t *testing.T, uri string) string {
	t.Helper()
	marker := restoreDestDir + "/"
	i := strings.Index(uri, marker)
	if i < 0 {
		t.Fatalf("artifact URI %q does not contain the destination subPath %q; "+
			"the destination is meant to be a directory the agent appends a filename to", uri, restoreDestDir)
	}
	return uri[i:]
}
