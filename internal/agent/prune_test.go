/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The prune agent's contract is narrow on purpose: delete exactly the one file
// the caller named, treat an already-absent file as done, and never touch
// anything else in the directory. The last one is what keeps a shared backup
// volume safe.

func setPruneEnv(t *testing.T, mount, subPath, name string) {
	t.Helper()
	t.Setenv(envDestKind, "pvc")
	t.Setenv(envPVCMountPath, mount)
	t.Setenv(envPVCSubPath, subPath)
	t.Setenv(envSnapshotName, name)
}

func TestRunPrune_DeletesNamedSnapshot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "backup-1.db")
	if err := os.WriteFile(target, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	setPruneEnv(t, dir, "", "backup-1")
	if err := RunPrune(context.Background()); err != nil {
		t.Fatalf("RunPrune: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("snapshot not deleted: %v", err)
	}
}

// The destination directory is frequently shared — one volume for several
// clusters. Pruning one snapshot must not disturb its neighbours.
func TestRunPrune_LeavesOtherSnapshotsAlone(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"backup-1.db", "backup-2.db", "someone-elses.db"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setPruneEnv(t, dir, "", "backup-1")
	if err := RunPrune(context.Background()); err != nil {
		t.Fatalf("RunPrune: %v", err)
	}

	for _, n := range []string{"backup-2.db", "someone-elses.db"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("prune removed %s, which it was not asked to touch: %v", n, err)
		}
	}
}

// The Job is retried on failure and re-run on a later reconcile; "already gone"
// must not turn into a permanently failing retention policy.
func TestRunPrune_MissingSnapshotIsSuccess(t *testing.T) {
	setPruneEnv(t, t.TempDir(), "", "never-existed")
	if err := RunPrune(context.Background()); err != nil {
		t.Fatalf("pruning an absent snapshot must succeed, got: %v", err)
	}
}

func TestRunPrune_HonoursSubPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "cluster-a")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "backup-1.db")
	if err := os.WriteFile(target, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A same-named file outside the subPath must survive: the subPath is what
	// separates one cluster's backups from another's on a shared volume.
	decoy := filepath.Join(dir, "backup-1.db")
	if err := os.WriteFile(decoy, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	setPruneEnv(t, dir, "cluster-a", "backup-1")
	if err := RunPrune(context.Background()); err != nil {
		t.Fatalf("RunPrune: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("snapshot under the subPath was not deleted")
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("prune escaped its subPath and deleted %s", decoy)
	}
}

func TestRunPrune_RequiresASnapshotName(t *testing.T) {
	t.Setenv(envDestKind, "pvc")
	t.Setenv(envPVCMountPath, t.TempDir())
	t.Setenv(envSnapshotName, "")

	if err := RunPrune(context.Background()); err == nil {
		t.Fatalf("expected an error with no snapshot named; a prune with no target must not be a silent no-op")
	}
}
