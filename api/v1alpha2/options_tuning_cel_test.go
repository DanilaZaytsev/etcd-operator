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

	lll "github.com/cozystack/etcd-operator/api/v1alpha2"
)

// The heartbeat/election ratio is the one tuning knob whose misconfiguration
// is invisible: too tight a ratio does not error anywhere, it just makes the
// cluster re-elect its leader whenever a write waits on the array. The
// apiserver is the only place that can catch it before it reaches production.

func ptr64(i int64) *int64 { return &i }

func TestCEL_ElectionTimeoutMustBeFiveTimesHeartbeat(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tuning-ratio")
	c.Spec.Options = &lll.EtcdOptions{
		HeartbeatIntervalMilliseconds: ptr64(500),
		ElectionTimeoutMilliseconds:   ptr64(2000), // 4x — below the 5x floor
	}

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted a 4x election/heartbeat ratio")
	}
	if !strings.Contains(err.Error(), "5x") {
		t.Fatalf("error did not mention the ratio: %v", err)
	}
}

func TestCEL_FiveTimesRatioIsAccepted(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tuning-ok")
	c.Spec.Options = &lll.EtcdOptions{
		HeartbeatIntervalMilliseconds: ptr64(500),
		ElectionTimeoutMilliseconds:   ptr64(2500),
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("apiserver rejected a valid 5x ratio: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })
}

// Raising the heartbeat alone is the trap: etcd's default election timeout is
// 1000ms, so a 500ms heartbeat set on its own is a 2x ratio that no rule
// comparing the two *set* fields would ever see.
func TestCEL_HeartbeatAloneIsRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tuning-half")
	c.Spec.Options = &lll.EtcdOptions{HeartbeatIntervalMilliseconds: ptr64(500)}

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted a heartbeat with no election timeout alongside it")
	}
	if !strings.Contains(err.Error(), "electionTimeoutMilliseconds") {
		t.Fatalf("error did not name the missing field: %v", err)
	}
}

// The reverse is safe: raising only the election timeout widens the ratio
// against the 100ms default heartbeat.
func TestCEL_ElectionTimeoutAloneIsAccepted(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tuning-election-only")
	c.Spec.Options = &lll.EtcdOptions{ElectionTimeoutMilliseconds: ptr64(5000)}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("apiserver rejected an election timeout set on its own: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })
}

func TestCEL_ElectionTimeoutUpperBound(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	// etcd itself refuses to start above 50s.
	c := validCluster("tuning-too-long")
	c.Spec.Options = &lll.EtcdOptions{
		HeartbeatIntervalMilliseconds: ptr64(10000),
		ElectionTimeoutMilliseconds:   ptr64(60000),
	}
	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted an election timeout above etcd's own 50s ceiling")
	}
}

func TestCEL_BackendBatchAndRetentionAccepted(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tuning-backend")
	c.Spec.Options = &lll.EtcdOptions{
		MaxWals:                          ptr64(3),
		MaxSnapshots:                     ptr64(3),
		BackendBatchLimit:                ptr64(5000),
		BackendBatchIntervalMilliseconds: ptr64(100),
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("apiserver rejected valid backend tuning: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })
}
