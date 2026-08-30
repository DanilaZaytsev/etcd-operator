/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha2_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// The storage-pool CEL rules, end-to-end against a real apiserver. These are
// the guard rails around a list whose entries own already-bound PVCs: what
// they have to get right is which edits are safe (append a pool, grow it,
// cordon it) and which would silently strand or orphan a volume (remove a
// pool, repoint it at another StorageClass, shrink it).

func poolsOf(names ...string) []lll.StoragePool {
	pools := make([]lll.StoragePool, 0, len(names))
	for _, n := range names {
		sc := "sc-" + n
		pools = append(pools, lll.StoragePool{Name: n, StorageClassName: &sc})
	}
	return pools
}

func clusterWithPools(name string, names ...string) *lll.EtcdCluster {
	c := validCluster(name)
	c.Spec.Storage.Pools = poolsOf(names...)
	return c
}

// Pools and storageClassName are two spellings of the same setting; accepting
// both would leave it ambiguous which one provisions the PVC.
func TestCEL_PoolsAndStorageClassNameMutuallyExclusive(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pools-and-sc", "a", "b")
	sc := "standalone"
	c.Spec.Storage.StorageClassName = &sc

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted pools together with storageClassName")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error did not mention mutual exclusion: %v", err)
	}
}

// A memory-backed member has no PVC, so there is nothing for a pool to place.
func TestCEL_PoolsRejectedWithMemoryMedium(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pools-memory", "a", "b")
	c.Spec.Storage.Medium = lll.StorageMediumMemory

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted pools with medium=Memory")
	}
	if !strings.Contains(err.Error(), "medium=Memory") {
		t.Fatalf("error did not mention the memory medium: %v", err)
	}
}

// Pool names are the label value and the placement key; two pools sharing one
// would make the member count ambiguous. Enforced by listType=map, not CEL.
func TestCEL_DuplicatePoolNamesRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("pools-dup")
	scA, scB := "sc-a", "sc-b"
	c.Spec.Storage.Pools = []lll.StoragePool{
		{Name: "array-a", StorageClassName: &scA},
		{Name: "array-a", StorageClassName: &scB},
	}

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted two pools with the same name")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Fatalf("error did not mention duplication: %v", err)
	}
}

// A cluster created without pools has its PVCs on the default/single
// StorageClass; introducing pools afterwards would claim a spread that the
// existing volumes do not have.
func TestCEL_PoolsCannotBeAddedToExistingCluster(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("pools-added")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create poolless cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools = poolsOf("a", "b")

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted pools added to an existing poolless cluster")
	}
	if !strings.Contains(err.Error(), "cannot be added to or removed from") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCEL_PoolsCannotBeRemovedWholesale(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pools-dropped", "a", "b")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools = nil

	if err := k8s.Update(ctx, got); err == nil {
		t.Fatalf("apiserver accepted dropping the whole pool list")
	}
}

// Removing one pool would orphan the PVCs already bound in it.
func TestCEL_ExistingPoolCannotBeRemoved(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-removed", "a", "b", "c")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools = poolsOf("a", "b")

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted removing a pool that already holds PVCs")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A PVC's storageClassName is itself immutable, so repointing the pool would
// mean the pool and its existing volumes disagree about where they live.
func TestCEL_ExistingPoolCannotBeRepointed(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-repointed", "a", "b")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	other := "sc-somewhere-else"
	got.Spec.Storage.Pools[0].StorageClassName = &other

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted repointing a pool at another StorageClass")
	}
	// Caught by the per-pool transition rule, not the list-level append-only
	// scan: listType=map correlates the pool with its old self by name.
	if !strings.Contains(err.Error(), "storageClassName is immutable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Taking on a fourth array must not require recreating the cluster — that is
// the entire reason the list is append-only rather than immutable.
func TestCEL_NewPoolCanBeAppended(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-appended", "a", "b", "c")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools = poolsOf("a", "b", "c", "d")

	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("apiserver rejected appending a new pool: %v", err)
	}
}

// Cordoning is the break-glass for a failed array; if it were rejected the
// replacement member would keep landing on the dead one.
func TestCEL_PoolCanBeCordoned(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-cordoned", "a", "b", "c")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools[1].Disabled = true

	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("apiserver rejected cordoning a pool: %v", err)
	}
}

func TestCEL_PoolSizeCanGrowButNotShrink(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-resize", "a", "b")
	small := resource.MustParse("1Gi")
	c.Spec.Storage.Pools[0].Size = &small
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	// Grow: allowed.
	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	big := resource.MustParse("4Gi")
	got.Spec.Storage.Pools[0].Size = &big
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("apiserver rejected growing a pool: %v", err)
	}

	// Shrink: rejected — a PVC cannot shrink.
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	back := resource.MustParse("2Gi")
	got.Spec.Storage.Pools[0].Size = &back
	if err := k8s.Update(ctx, got); err == nil {
		t.Fatalf("apiserver accepted shrinking a pool's size")
	}

	// Dropping the override is a shrink in disguise: the member would fall
	// back to the (smaller) cluster-wide size.
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage.Pools[0].Size = nil
	if err := k8s.Update(ctx, got); err == nil {
		t.Fatalf("apiserver accepted dropping a pool's size override")
	}
}

// The happy path: a three-array cluster is a legal spec.
func TestCEL_ThreePoolClusterIsAccepted(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := clusterWithPools("pool-happy", "array-a", "array-b", "array-c")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("apiserver rejected a valid three-pool cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Storage.Pools) != 3 {
		t.Fatalf("round-tripped %d pools, want 3", len(got.Spec.Storage.Pools))
	}
}
