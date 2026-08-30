package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// What matters in these tests is not the shape of the returned struct but the
// placement decision: which storage array a member's PVC lands on. Getting it
// wrong either concentrates a cluster's members on one array (so losing that
// array loses quorum — the exact failure the feature exists to prevent) or
// strands a replacement on a dead one.

func poolsFixture() []lll.StoragePool {
	sanA, sanB, sanC := "san-a", "san-b", "san-c"
	return []lll.StoragePool{
		{Name: "array-a", StorageClassName: &sanA},
		{Name: "array-b", StorageClassName: &sanB},
		{Name: "array-c", StorageClassName: &sanC},
	}
}

func memberInPool(name, pool string) lll.EtcdMember {
	return lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       lll.EtcdMemberSpec{StoragePool: pool},
	}
}

func TestResolveStoragePool_NoPoolsIsPassthrough(t *testing.T) {
	sc := "default-sc"
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), StorageClassName: &sc}

	got, pool, err := resolveStoragePool(observed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "" {
		t.Fatalf("pool = %q, want empty for a cluster that declares no pools", pool)
	}
	if got.StorageClassName == nil || *got.StorageClassName != sc {
		t.Fatalf("storageClassName not preserved: %+v", got.StorageClassName)
	}
	if got.Size.Cmp(observed.Size) != 0 {
		t.Fatalf("size not preserved: %s", got.Size.String())
	}
}

// The seed must land in pools[0] with no members present: a restore recreates
// the cluster from scratch, and a seed that lands somewhere different each time
// makes the DR runbook unreproducible.
func TestResolveStoragePool_SeedIsDeterministic(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}

	for i := 0; i < 5; i++ {
		got, pool, err := resolveStoragePool(observed, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pool != "array-a" {
			t.Fatalf("seed landed in %q, want array-a (declaration order breaks the all-zero tie)", pool)
		}
		if got.StorageClassName == nil || *got.StorageClassName != "san-a" {
			t.Fatalf("seed storageClassName = %v, want san-a", got.StorageClassName)
		}
	}
}

// Three replicas over three pools must be 1/1/1. Any other split means one
// array holds two members and can take quorum with it.
func TestResolveStoragePool_ThreeReplicasSpreadOnePerPool(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}

	var members []lll.EtcdMember
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		_, pool, err := resolveStoragePool(observed, members)
		if err != nil {
			t.Fatalf("member %d: %v", i, err)
		}
		seen[pool]++
		members = append(members, memberInPool("m", pool))
	}
	for _, name := range []string{"array-a", "array-b", "array-c"} {
		if seen[name] != 1 {
			t.Fatalf("pool %s holds %d members, want exactly 1 (got layout %v)", name, seen[name], seen)
		}
	}
}

// A replacement must land back on the array the lost member vacated, so the
// spread self-heals without any explicit rebalancing pass.
func TestResolveStoragePool_ReplacementRefillsTheVacatedPool(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}
	members := []lll.EtcdMember{
		memberInPool("m1", "array-a"),
		memberInPool("m3", "array-c"),
	}

	_, pool, err := resolveStoragePool(observed, members)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "array-b" {
		t.Fatalf("replacement landed in %q, want array-b (the vacated pool)", pool)
	}
}

// A member being deleted has already released its PVC (the PVC is
// controller-owned by the member), so its pool is free for the replacement.
// Counting it would push the replacement onto a different array and leave the
// layout permanently lopsided.
func TestResolveStoragePool_TerminatingMemberDoesNotHoldItsPool(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}
	now := metav1.Now()
	dying := memberInPool("m2", "array-b")
	dying.DeletionTimestamp = &now
	dying.Finalizers = []string{MemberFinalizer}

	members := []lll.EtcdMember{
		memberInPool("m1", "array-a"),
		dying,
		memberInPool("m3", "array-c"),
	}

	_, pool, err := resolveStoragePool(observed, members)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "array-b" {
		t.Fatalf("replacement landed in %q, want array-b (the terminating member's pool)", pool)
	}
}

// A dormant member keeps its PVC across the pause, so its array is still
// occupied and must still be counted.
func TestResolveStoragePool_DormantMemberStillHoldsItsPool(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}
	dormant := memberInPool("m1", "array-a")
	dormant.Spec.Dormant = true

	_, pool, err := resolveStoragePool(observed, []lll.EtcdMember{dormant})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool == "array-a" {
		t.Fatalf("new member placed on array-a, which the dormant member's PVC still occupies")
	}
}

// The cordon is the whole point of Disabled: without it a member lost to a
// dead array is replaced straight back onto it (that array is the least-used
// pool) and the replacement's PVC sits Pending forever.
func TestResolveStoragePool_DisabledPoolIsSkipped(t *testing.T) {
	pools := poolsFixture()
	pools[1].Disabled = true
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: pools}

	members := []lll.EtcdMember{
		memberInPool("m1", "array-a"),
		memberInPool("m3", "array-c"),
	}

	_, pool, err := resolveStoragePool(observed, members)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool == "array-b" {
		t.Fatalf("replacement landed on the cordoned array-b")
	}
	if pool != "array-a" && pool != "array-c" {
		t.Fatalf("replacement landed in unexpected pool %q", pool)
	}
}

func TestResolveStoragePool_AllPoolsDisabledIsAnError(t *testing.T) {
	pools := poolsFixture()
	for i := range pools {
		pools[i].Disabled = true
	}
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: pools}

	_, _, err := resolveStoragePool(observed, nil)
	if err == nil {
		t.Fatalf("expected an error when every pool is cordoned")
	}
	// The message has to name the pools — an operator reading it mid-incident
	// needs to know which cordon to lift.
	for _, name := range []string{"array-a", "array-b", "array-c"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error does not name pool %s: %v", name, err)
		}
	}
}

func TestResolveStoragePool_PoolSizeOverridesClusterSize(t *testing.T) {
	pools := poolsFixture()
	big := resource.MustParse("40Gi")
	pools[0].Size = &big
	observed := lll.StorageSpec{Size: resource.MustParse("20Gi"), Pools: pools}

	got, pool, err := resolveStoragePool(observed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "array-a" {
		t.Fatalf("pool = %q, want array-a", pool)
	}
	if got.Size.Cmp(big) != 0 {
		t.Fatalf("size = %s, want the pool override 40Gi", got.Size.String())
	}
}

// A member is placed, it does not re-decide: carrying the pool list onto the
// member would invite the member controller to resolve it a second time and
// disagree with the cluster controller.
func TestResolveStoragePool_MemberStorageCarriesNoPoolList(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("1Gi"), Pools: poolsFixture()}

	got, _, err := resolveStoragePool(observed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Pools != nil {
		t.Fatalf("member storage carries a pool list: %+v", got.Pools)
	}
	// ...and the caller's spec must be untouched.
	if len(observed.Pools) != 3 {
		t.Fatalf("resolveStoragePool mutated the observed spec: %+v", observed.Pools)
	}
}

func TestStoragePoolAffinity(t *testing.T) {
	clusterAff := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	poolAff := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}

	pools := poolsFixture()
	pools[1].Affinity = poolAff
	observed := lll.StorageSpec{Pools: pools}

	if got := storagePoolAffinity(observed, "array-b", clusterAff); got != poolAff {
		t.Fatalf("pool affinity did not win for its own member")
	}
	if got := storagePoolAffinity(observed, "array-a", clusterAff); got != clusterAff {
		t.Fatalf("pool without an override must inherit the cluster-wide affinity")
	}
	if got := storagePoolAffinity(observed, "", clusterAff); got != clusterAff {
		t.Fatalf("poolless cluster must keep the cluster-wide affinity")
	}
}

func TestUnevenStoragePools(t *testing.T) {
	tests := []struct {
		name     string
		pools    []lll.StoragePool
		replicas int32
		want     bool
	}{
		{"3 replicas over 3 pools is even", poolsFixture(), 3, false},
		{"6 replicas over 3 pools is even", poolsFixture(), 6, false},
		{"3 replicas over 2 pools is uneven", poolsFixture()[:2], 3, true},
		{"5 replicas over 3 pools is uneven", poolsFixture(), 5, true},
		{"a single pool is never uneven", poolsFixture()[:1], 3, false},
		// A single-member tenant declaring three arrays is under-filled, not
		// lopsided — it must not be warned about. This is the shape of a
		// tenant that starts at one replica and grows to three.
		{"one replica over three pools is not uneven", poolsFixture(), 1, false},
		{"one replica over one pool is not uneven", poolsFixture()[:1], 1, false},
		{"no pools is never uneven", nil, 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, got := unevenStoragePools(lll.StorageSpec{Pools: tc.pools}, tc.replicas)
			if got != tc.want {
				t.Fatalf("uneven = %v, want %v (msg %q)", got, tc.want, msg)
			}
			if got && msg == "" {
				t.Fatalf("flagged uneven with no message to surface")
			}
		})
	}
}

// A cordoned pool is not a placement target, so it must not count toward the
// evenness arithmetic either: 3 replicas over 3 pools with one cordoned is
// 3-over-2, which IS uneven and worth saying.
func TestUnevenStoragePools_IgnoresCordonedPools(t *testing.T) {
	pools := poolsFixture()
	pools[2].Disabled = true

	if _, uneven := unevenStoragePools(lll.StorageSpec{Pools: pools}, 3); !uneven {
		t.Fatalf("3 replicas over 2 enabled pools should be flagged uneven")
	}
}

func TestWithStoragePoolLabel(t *testing.T) {
	l := withStoragePoolLabel(map[string]string{"a": "b"}, "array-a")
	if l[LabelStoragePool] != "array-a" {
		t.Fatalf("label not stamped: %v", l)
	}
	// A poolless cluster gets no label at all rather than an empty value —
	// an empty-valued label would match selectors that mean "has a pool".
	l2 := withStoragePoolLabel(map[string]string{"a": "b"}, "")
	if _, ok := l2[LabelStoragePool]; ok {
		t.Fatalf("empty pool stamped a label: %v", l2)
	}
}

// ── Controller wiring ────────────────────────────────────────────────────
//
// resolveStoragePool is tested above in isolation; these cover that the two
// member-creation sites actually call it and stamp what they resolved. A
// placement decision the controller never records is a placement that does not
// happen.

func TestBootstrap_PlacesSeedInFirstPool(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  lll.StorageSpec{Size: quickQty(t, "256Mi"), Pools: poolsFixture()},
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef))}

	reconcileUntilStable(t, r, c, "test", "ns", 8)

	// The locked target must carry the pools, or a later cordon could never
	// reach the placement logic.
	got := mustGet(t, c, "test", "ns", &lll.EtcdCluster{})
	if got.Status.Observed == nil || len(got.Status.Observed.Storage.Pools) != 3 {
		t.Fatalf("Observed.Storage.Pools not latched: %+v", got.Status.Observed)
	}

	members := &lll.EtcdMemberList{}
	if err := c.List(ctx, members, client.InNamespace("ns")); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(members.Items) != 1 {
		t.Fatalf("expected exactly one seed member; got %d", len(members.Items))
	}
	seed := members.Items[0]

	if seed.Spec.StoragePool != "array-a" {
		t.Fatalf("seed StoragePool = %q, want array-a (deterministic for a reproducible restore)", seed.Spec.StoragePool)
	}
	if seed.Spec.Storage.StorageClassName == nil || *seed.Spec.Storage.StorageClassName != "san-a" {
		t.Fatalf("seed storageClassName = %v, want san-a", seed.Spec.Storage.StorageClassName)
	}
	if seed.Labels[LabelStoragePool] != "array-a" {
		t.Fatalf("seed missing the storage-pool label: %v", seed.Labels)
	}
	if seed.Spec.Storage.Pools != nil {
		t.Fatalf("seed carries the cluster's pool list; it should carry only its resolved backend")
	}
}

func TestScaleUp_PlacesNewMemberInAnUnusedPool(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: types.UID("cluster-uid")},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(2),
			Version:  "3.5.17",
			Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: poolsFixture()},
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 2,
				Version:  "3.5.17",
				Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: poolsFixture()},
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	seed := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-seed", Namespace: "ns", Labels: memberLabels("test", "test-seed")},
		Spec: lll.EtcdMemberSpec{
			ClusterName: "test", Version: "3.5.17", InitialCluster: "x", ClusterToken: "ns-test-x",
			StoragePool: "array-a",
		},
		Status: lll.EtcdMemberStatus{
			PodName: "test-seed", MemberID: "a01", IsVoter: true,
			Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}},
		},
	}
	c, _ := newTestClient(t, cluster, seed)
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-seed", PeerURLs: []string{peerURL("http", "test-seed", "test", "ns")}},
	)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.scaleUp(ctx, cluster, []lll.EtcdMember{*seed}); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}

	all := &lll.EtcdMemberList{}
	_ = c.List(ctx, all, client.InNamespace("ns"))
	var fresh *lll.EtcdMember
	for i := range all.Items {
		if all.Items[i].Name != seed.Name {
			fresh = &all.Items[i]
			break
		}
	}
	if fresh == nil {
		t.Fatalf("scaleUp must create a second member; got only %d", len(all.Items))
	}
	if fresh.Spec.StoragePool == "array-a" {
		t.Fatalf("second member landed on array-a alongside the seed; both would die with that array")
	}
	if fresh.Spec.StoragePool != "array-b" {
		t.Fatalf("second member StoragePool = %q, want array-b", fresh.Spec.StoragePool)
	}
	if fresh.Spec.Storage.StorageClassName == nil || *fresh.Spec.Storage.StorageClassName != "san-b" {
		t.Fatalf("second member storageClassName = %v, want san-b", fresh.Spec.Storage.StorageClassName)
	}
	if fresh.Labels[LabelStoragePool] != "array-b" {
		t.Fatalf("second member missing the storage-pool label: %v", fresh.Labels)
	}
}

// The cordon has to survive the locking pattern: if specEqualsObserved ignored
// Pools, flipping disabled=true would never be latched into the observed
// target, so the replacement member would keep being placed on the dead array
// — the cordon would look applied and do nothing.
func TestSpecEqualsObserved_PoolCordonMatters(t *testing.T) {
	cordoned := poolsFixture()
	cordoned[1].Disabled = true

	cluster := &lll.EtcdCluster{
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: cordoned},
		},
		Status: lll.EtcdClusterStatus{
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: poolsFixture()},
			},
		},
	}
	if specEqualsObserved(cluster) {
		t.Fatalf("specEqualsObserved must return false when a pool has been cordoned")
	}

	cluster.Status.Observed.Storage.Pools = cordoned
	if !specEqualsObserved(cluster) {
		t.Fatalf("specEqualsObserved must return true once the cordon is latched")
	}
}

// Appending a pool is likewise a target change: the new array must become
// available to the next member, not sit unused until something else moves.
func TestSpecEqualsObserved_AppendedPoolMatters(t *testing.T) {
	cluster := &lll.EtcdCluster{
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: poolsFixture()},
		},
		Status: lll.EtcdClusterStatus{
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  lll.StorageSpec{Size: quickQty(t, "1Gi"), Pools: poolsFixture()[:2]},
			},
		},
	}
	if specEqualsObserved(cluster) {
		t.Fatalf("specEqualsObserved must return false when a pool has been appended")
	}
}

// ── Storage tuning flags ─────────────────────────────────────────────────

// The flags themselves are what the operator's SAN tuning story reduces to:
// if they are not on the command line, the CRD field is decoration.
func TestOptionFlags_StorageTuning(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	o := &lll.EtcdOptions{
		HeartbeatIntervalMilliseconds:    i(500),
		ElectionTimeoutMilliseconds:      i(2500),
		MaxWals:                          i(3),
		MaxSnapshots:                     i(4),
		BackendBatchLimit:                i(5000),
		BackendBatchIntervalMilliseconds: i(100),
	}
	got := strings.Join(optionFlags(o), " ")

	// etcd accepts a bare integer as milliseconds for the two durations, but
	// the rendered command line has to be readable on the Pod, so the unit is
	// spelled out.
	for _, want := range []string{
		"--heartbeat-interval=500ms",
		"--election-timeout=2500ms",
		"--max-wals=3",
		"--max-snapshots=4",
		"--backend-batch-limit=5000",
		"--backend-batch-interval=100ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in rendered flags: %s", want, got)
		}
	}
}

// An unset option must emit nothing rather than a zero value: rendering
// --heartbeat-interval=0ms would be worse than etcd's default.
func TestOptionFlags_UnsetTuningEmitsNothing(t *testing.T) {
	got := strings.Join(optionFlags(&lll.EtcdOptions{}), " ")
	for _, unwanted := range []string{"heartbeat-interval", "election-timeout", "max-wals", "max-snapshots", "backend-batch"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("unset option rendered a flag: %s", got)
		}
	}
}

func TestWorkerCount(t *testing.T) {
	// 0 is "unset", which must mean controller-runtime's own default of one
	// worker rather than being passed through as 0 (which panics at startup).
	if got := workerCount(0); got != 1 {
		t.Fatalf("workerCount(0) = %d, want 1", got)
	}
	// A negative value cannot be rejected by the flag parser, so it is
	// normalised here rather than reaching controller-runtime.
	if got := workerCount(-4); got != 1 {
		t.Fatalf("workerCount(-4) = %d, want 1", got)
	}
	if got := workerCount(8); got != 8 {
		t.Fatalf("workerCount(8) = %d, want 8", got)
	}
}

// The single-member tenant shape, end to end through the placement logic: one
// member, three declared arrays. It lands on the first and the other two stay
// free for a later scale-up — which then fills them one at a time.
func TestResolveStoragePool_SingleMemberThenGrowth(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("20Gi"), Pools: poolsFixture()}

	got, pool, err := resolveStoragePool(observed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "array-a" {
		t.Fatalf("the single member landed in %q, want array-a", pool)
	}
	if got.StorageClassName == nil || *got.StorageClassName != "san-a" {
		t.Fatalf("storageClassName = %v, want san-a", got.StorageClassName)
	}

	// Scale 1 -> 3: the two idle arrays fill, one member each.
	members := []lll.EtcdMember{memberInPool("m1", pool)}
	seen := map[string]int{pool: 1}
	for i := 0; i < 2; i++ {
		_, p, err := resolveStoragePool(observed, members)
		if err != nil {
			t.Fatalf("scale-up %d: %v", i, err)
		}
		seen[p]++
		members = append(members, memberInPool("m", p))
	}
	for _, name := range []string{"array-a", "array-b", "array-c"} {
		if seen[name] != 1 {
			t.Fatalf("after growing to 3, pool %s holds %d members, want 1 (layout %v)", name, seen[name], seen)
		}
	}
}

// A single member with a single declared array is the other legitimate shape:
// no spread to be had, and nothing should complain about it.
func TestResolveStoragePool_SingleMemberSinglePool(t *testing.T) {
	observed := lll.StorageSpec{Size: resource.MustParse("20Gi"), Pools: poolsFixture()[:1]}

	_, pool, err := resolveStoragePool(observed, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != "array-a" {
		t.Fatalf("pool = %q, want array-a", pool)
	}
	if msg, uneven := unevenStoragePools(observed, 1); uneven {
		t.Fatalf("a one-member, one-array cluster was flagged uneven: %s", msg)
	}
}
