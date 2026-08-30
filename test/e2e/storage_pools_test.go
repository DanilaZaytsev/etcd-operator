//go:build e2e

package e2e

import (
	"context"
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha2 "github.com/cozystack/etcd-operator/api/v1alpha2"
)

const (
	poolsNamespace = "pools-e2e"
	poolsCluster   = "etcd"
	poolLabel      = "etcd-operator.cozystack.io/storage-pool"
)

// TestStoragePoolsPlaceAndCordon covers the reason storage pools exist: a
// cluster spread over several arrays must lose one member, not quorum, when an
// array goes — and when one does go, the operator must be steerable away from
// it rather than parking replacements on dead storage forever.
//
// Placement is count-based rather than ordinal, because members are named with
// GenerateName and there is no index to key a pool off. That makes the property
// worth asserting on a live cluster: unit tests can prove the selection function
// picks the least-used pool, but only a real reconcile proves the operator
// applies the choice to the Pod and PVC it actually creates.
//
// Both pools point at the same StorageClass. The pool a member lands in is
// recorded as a label and drives affinity, not the class name, so one class is
// enough to exercise placement — and kind has exactly one to offer.
func TestStoragePoolsPlaceAndCordon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	createNamespace(ctx, t, poolsNamespace)

	three := int32(3)
	ec := &etcdv1alpha2.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: poolsCluster, Namespace: poolsNamespace},
		Spec: etcdv1alpha2.EtcdClusterSpec{
			Replicas: &three,
			Version:  "3.6.11",
			Storage: etcdv1alpha2.StorageSpec{
				Size: resource.MustParse("1Gi"),
				Pools: []etcdv1alpha2.StoragePool{
					{Name: "array-a"},
					{Name: "array-b"},
					{Name: "array-c"},
				},
			},
		},
	}
	if err := kube.Create(ctx, ec); err != nil {
		t.Fatalf("create EtcdCluster with pools: %v", err)
	}
	waitFor(ctx, t, 5*time.Minute, "cluster Available", etcdClusterAvailable(poolsNamespace, poolsCluster))
	waitFor(ctx, t, 3*time.Minute, "3 members ready", readyMembersIsIn(poolsNamespace, poolsCluster, 3))

	// One member per pool is the whole point: any other distribution means an
	// array outage takes more than one member with it.
	byPool := membersByPool(ctx, t, poolsNamespace, poolsCluster)
	for _, pool := range []string{"array-a", "array-b", "array-c"} {
		if byPool[pool] != 1 {
			t.Fatalf("pool %q holds %d members, want exactly 1 (distribution: %v). "+
				"Two members on one array means losing that array costs quorum, "+
				"which is the failure pools exist to prevent", pool, byPool[pool], byPool)
		}
	}
	t.Logf("seed placement is one member per pool: %v", byPool)

	// Cordon array-a. This is the move an operator makes when an array has
	// failed: without it the dead pool stays the least-used one and therefore
	// keeps attracting every replacement, whose PVC then sits Pending forever.
	live := &etcdv1alpha2.EtcdCluster{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: poolsNamespace, Name: poolsCluster}, live); err != nil {
		t.Fatalf("get cluster before cordon: %v", err)
	}
	five := int32(5)
	live.Spec.Replicas = &five
	for i := range live.Spec.Storage.Pools {
		if live.Spec.Storage.Pools[i].Name == "array-a" {
			live.Spec.Storage.Pools[i].Disabled = true
		}
	}
	if err := kube.Update(ctx, live); err != nil {
		t.Fatalf("cordon array-a and scale to 5: %v", err)
	}

	waitFor(ctx, t, 5*time.Minute, "5 members ready", readyMembersIsIn(poolsNamespace, poolsCluster, 5))

	// The two added members must both have avoided the cordoned pool. Scaling
	// rather than deleting a member keeps the assertion deterministic: it does
	// not depend on the self-heal predicate firing.
	byPool = membersByPool(ctx, t, poolsNamespace, poolsCluster)
	if byPool["array-a"] != 1 {
		t.Fatalf("cordoned pool array-a holds %d members, want the original 1 (distribution: %v). "+
			"A cordon that still attracts new members is not a cordon — a replacement "+
			"placed on a failed array leaves its PVC Pending indefinitely", byPool["array-a"], byPool)
	}
	if byPool["array-b"]+byPool["array-c"] != 4 {
		t.Fatalf("expected the 4 non-cordoned members split across array-b and array-c, got %v", byPool)
	}
	t.Logf("after cordoning array-a and scaling to 5: %v", byPool)

	// Cordoning must not evict what is already there. Existing members keep
	// their PVCs; the flag only governs where NEW ones go.
	if _, ok := byPool["array-a"]; !ok {
		t.Fatalf("cordoning array-a removed its existing member; a cordon governs placement of "+
			"new members only, and evicting a live member would cost availability for free (%v)", byPool)
	}
}

// membersByPool counts the cluster's members per storage pool, reading the
// label the operator stamps at placement time.
func membersByPool(ctx context.Context, t *testing.T, namespace, cluster string) map[string]int {
	t.Helper()
	list := &etcdv1alpha2.EtcdMemberList{}
	if err := kube.List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabels{"etcd-operator.cozystack.io/cluster": cluster}); err != nil {
		t.Fatalf("list members: %v", err)
	}
	out := map[string]int{}
	var unplaced []string
	for i := range list.Items {
		m := &list.Items[i]
		pool, ok := m.Labels[poolLabel]
		if !ok || pool == "" {
			unplaced = append(unplaced, m.Name)
			continue
		}
		out[pool]++
	}
	if len(unplaced) > 0 {
		sort.Strings(unplaced)
		t.Fatalf("members %v carry no %s label: a member the operator cannot attribute to a pool "+
			"is a member no cordon can steer", unplaced, poolLabel)
	}
	return out
}
