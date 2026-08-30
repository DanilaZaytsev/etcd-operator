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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// Names here are the head of a derivation chain that ends at a Job, and the
// apiserver caps a Job name at 63 characters because the Job controller copies
// it into a Pod label. Every link has to be checked at the point the user types
// the name, because the failure otherwise surfaces two objects away: the policy
// stamps snapshots perfectly well and every one of them fails to run.
//
//	policy(44) + "-" + tick(8) = snapshot(53) + "-snapshot" = job(62)

const (
	maxSnapshotPolicyName = 44
	maxSnapshotName       = 54
	maxJobName            = 63
	tickDigits            = 8
)

// The arithmetic itself, so a later change to one cap cannot silently break the
// chain without this failing.
func TestNameChainFitsAJobName(t *testing.T) {
	stamped := maxSnapshotPolicyName + len("-") + tickDigits
	if stamped > maxSnapshotName {
		t.Fatalf("a policy at its cap stamps a %d-char snapshot, over the %d-char snapshot cap", stamped, maxSnapshotName)
	}
	for _, suffix := range []string{"-snapshot", "-prune"} {
		if got := maxSnapshotName + len(suffix); got > maxJobName {
			t.Fatalf("a snapshot at its cap yields a %d-char %q Job name, over the apiserver's %d", got, suffix, maxJobName)
		}
	}
}

func snapshotPolicyNamed(n int) *lll.EtcdSnapshotPolicy {
	return &lll.EtcdSnapshotPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("p", n), Namespace: "default"},
		Spec: lll.EtcdSnapshotPolicySpec{
			ClusterRef:  corev1.LocalObjectReference{Name: "c1"},
			Schedule:    lll.DefragSchedule{Cron: "0 * * * *"},
			Destination: lll.SnapshotLocation{PVC: &lll.PVCSnapshotLocation{ClaimName: "backups"}},
		},
	}
}

func TestCEL_SnapshotPolicyNameIsCapped(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	ok := snapshotPolicyNamed(maxSnapshotPolicyName)
	if err := k8s.Create(ctx, ok); err != nil {
		t.Fatalf("apiserver rejected a policy at the cap: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ok) })

	tooLong := snapshotPolicyNamed(maxSnapshotPolicyName + 1)
	err := k8s.Create(ctx, tooLong)
	if err == nil {
		_ = k8s.Delete(ctx, tooLong)
		t.Fatalf("apiserver accepted a %d-char policy name; its snapshots' Jobs would exceed 63 characters and never run",
			maxSnapshotPolicyName+1)
	}
	if !strings.Contains(err.Error(), "44 characters or fewer") {
		t.Fatalf("error did not name the cap: %v", err)
	}
}

func snapshotNamed(n int) *lll.EtcdSnapshot {
	return &lll.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("s", n), Namespace: "default"},
		Spec: lll.EtcdSnapshotSpec{
			ClusterRef:  corev1.LocalObjectReference{Name: "c1"},
			Destination: lll.SnapshotLocation{PVC: &lll.PVCSnapshotLocation{ClaimName: "backups"}},
		},
	}
}

func TestCEL_SnapshotNameIsCapped(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	ok := snapshotNamed(maxSnapshotName)
	if err := k8s.Create(ctx, ok); err != nil {
		t.Fatalf("apiserver rejected a snapshot at the cap: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, ok) })

	tooLong := snapshotNamed(maxSnapshotName + 1)
	err := k8s.Create(ctx, tooLong)
	if err == nil {
		_ = k8s.Delete(ctx, tooLong)
		t.Fatalf("apiserver accepted a %d-char snapshot name; its Job would exceed 63 characters and never be created",
			maxSnapshotName+1)
	}
	if !strings.Contains(err.Error(), "54 characters or fewer") {
		t.Fatalf("error did not name the cap: %v", err)
	}
}

// A policy at its cap must stamp a snapshot the apiserver still accepts —
// the two caps have to agree, not merely each be defensible on its own.
func TestCEL_PolicyAtItsCapStampsAValidSnapshot(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	// The controller's own naming: fmt.Sprintf("%s-%d", pol.Name, tick.Unix()/60).
	name := fmt.Sprintf("%s-%d", strings.Repeat("p", maxSnapshotPolicyName), 29783333)
	stamped := &lll.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: lll.EtcdSnapshotSpec{
			ClusterRef:  corev1.LocalObjectReference{Name: "c1"},
			Destination: lll.SnapshotLocation{PVC: &lll.PVCSnapshotLocation{ClaimName: "backups"}},
		},
	}
	if err := k8s.Create(ctx, stamped); err != nil {
		t.Fatalf("a policy at its cap stamps a snapshot the apiserver rejects: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, stamped) })
}
