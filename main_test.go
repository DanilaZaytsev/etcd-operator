/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cozystack/etcd-operator/controllers"
)

func TestDiscoverClusterDomain(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name: "standard kubelet-injected resolv.conf",
			contents: `search default.svc.cluster.local svc.cluster.local cluster.local
nameserver 10.96.0.10
options ndots:5
`,
			want: "cluster.local",
		},
		{
			name: "cozystack",
			contents: `search myns.svc.cozy.local svc.cozy.local cozy.local
nameserver 10.96.0.10
options ndots:5
`,
			want: "cozy.local",
		},
		{
			name: "multi-segment cluster domain",
			contents: `search myns.svc.example.internal svc.example.internal example.internal
nameserver 10.96.0.10
`,
			want: "example.internal",
		},
		{
			name: "comments and blank lines",
			contents: `# This is the cluster resolv.conf injected by kubelet.

search   myns.svc.cluster.local   svc.cluster.local   cluster.local
nameserver 10.96.0.10
`,
			want: "cluster.local",
		},
		{
			name: "host-style resolv.conf — no .svc suffix",
			contents: `search example.com
nameserver 8.8.8.8
`,
			want: "",
		},
		{
			name: "no search line at all",
			contents: `nameserver 8.8.8.8
`,
			want: "",
		},
		{
			name:     "empty file",
			contents: "",
			want:     "",
		},
		{
			name: "svc-only entry (degenerate, should not match)",
			contents: `search svc.
nameserver 10.96.0.10
`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "resolv.conf")
			if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got := discoverClusterDomain(path)
			if got != tc.want {
				t.Fatalf("discoverClusterDomain = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestDiscoverClusterDomain_MissingFile covers the running-outside-a-pod
// case (no /etc/resolv.conf at all, or the operator binary running on a
// developer laptop): the function returns "" so callers fall back to
// the explicit --cluster-domain flag or the default.
func TestDiscoverClusterDomain_MissingFile(t *testing.T) {
	if got := discoverClusterDomain("/no/such/file/anywhere"); got != "" {
		t.Fatalf("missing file: got %q; want empty", got)
	}
}

func TestWatchNamespaceCacheOptions(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]cache.Config
	}{
		{
			name: "empty means all namespaces",
			raw:  "",
			want: nil,
		},
		{
			name: "whitespace only means all namespaces",
			raw:  "   ",
			want: nil,
		},
		{
			name: "separators only means all namespaces",
			raw:  " , ,, ",
			want: nil,
		},
		{
			name: "single namespace",
			raw:  "tenant-a",
			want: map[string]cache.Config{"tenant-a": {}},
		},
		{
			name: "multiple namespaces",
			raw:  "tenant-a,tenant-b",
			want: map[string]cache.Config{"tenant-a": {}, "tenant-b": {}},
		},
		{
			name: "whitespace and empty entries are dropped",
			raw:  " tenant-a , ,tenant-b, ",
			want: map[string]cache.Config{"tenant-a": {}, "tenant-b": {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := watchNamespaceCacheOptions(tc.raw)
			if tc.want == nil {
				// No namespace scoping: watch everywhere. The per-object
				// selectors below still apply — "all namespaces" must not mean
				// "every Pod in the cluster".
				if got.DefaultNamespaces != nil {
					t.Fatalf("watchNamespaceCacheOptions(%q).DefaultNamespaces = %+v; want nil", tc.raw, got.DefaultNamespaces)
				}
			} else if !reflect.DeepEqual(got.DefaultNamespaces, tc.want) {
				t.Fatalf("watchNamespaceCacheOptions(%q).DefaultNamespaces = %+v; want %+v", tc.raw, got.DefaultNamespaces, tc.want)
			}
			// The object selectors are not a function of the namespace list and
			// must be present either way.
			if len(got.ByObject) == 0 {
				t.Fatalf("watchNamespaceCacheOptions(%q) left the cache unfiltered", tc.raw)
			}
		})
	}
}

func TestOperatorImageError(t *testing.T) {
	if err := operatorImageError(placeholderOperatorImage); err == nil {
		t.Errorf("operatorImageError(%q) = nil, want error (placeholder must be rejected)", placeholderOperatorImage)
	}
	// A real image ref and empty (snapshots simply unavailable) are both allowed.
	for _, img := range []string{"registry.example.com/etcd-operator:v1.2.3", ""} {
		if err := operatorImageError(img); err != nil {
			t.Errorf("operatorImageError(%q) = %v, want nil", img, err)
		}
	}
}

// The cache selectors decide how much of the cluster this operator holds in
// memory. On a parent cluster where every namespace is a tenant control plane,
// an unfiltered Pod cache is every tenant's apiserver, scheduler and
// controller-manager Pod — a footprint that scales with the whole cluster
// rather than with the number of etcd clusters.
func TestOperatorOwnedCacheSelectors(t *testing.T) {
	sel := operatorOwnedCacheSelectors()

	// The map is keyed by client.Object values, so look entries up by type
	// rather than by identity — two &corev1.Pod{} pointers are not equal.
	byType := map[reflect.Type]cache.ByObject{}
	for obj, cfg := range sel {
		byType[reflect.TypeOf(obj)] = cfg
	}

	// The core types the operator creates, all of which carry its cluster
	// label. Missing one here silently restores the unfiltered cache for it.
	for _, obj := range []client.Object{
		&corev1.Pod{},
		&corev1.PersistentVolumeClaim{},
		&corev1.Service{},
		&policyv1.PodDisruptionBudget{},
		&batchv1.Job{},
	} {
		byObj, ok := byType[reflect.TypeOf(obj)]
		if !ok {
			t.Fatalf("%T is cached unfiltered", obj)
		}
		if byObj.Label == nil {
			t.Fatalf("%T has no label selector", obj)
		}
		// An object of ours matches; a foreign one does not.
		if !byObj.Label.Matches(labels.Set{controllers.LabelCluster: "tenant-a"}) {
			t.Fatalf("%T selector rejects an operator-owned object", obj)
		}
		if byObj.Label.Matches(labels.Set{"app": "kube-apiserver"}) {
			t.Fatalf("%T selector admits a foreign object; the cache would hold the whole cluster", obj)
		}
	}

	// Secrets must NOT be here: they are excluded from the cache entirely
	// (client.CacheOptions.DisableFor), because user-provided TLS and S3
	// Secrets carry no operator label to select on. A selector here would
	// silently filter out every Secret the operator needs to read.
	if _, ok := byType[reflect.TypeOf(&corev1.Secret{})]; ok {
		t.Fatalf("Secrets have a cache selector; they are read live and would be filtered out entirely")
	}
}
