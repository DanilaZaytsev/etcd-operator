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
)

// The operator names the cluster's headless Service after metadata.name and
// the client Service "<name>-client". A Service name is a DNS-1035 label, so a
// cluster name that starts with a digit produces a Service the apiserver
// rejects, and the controller loops on "failed to ensure services" forever.
// The CRD must reject such a name at admission, where the error reaches the
// user. Found on a real kube-in-kube run: tenant namespace "36carcn".
func TestCEL_ClusterNameMustBeDNS1035Label(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	rejected := map[string]string{
		"digit-leading":   "36carcn-etcd",
		"underscore":      "tenant_etcd",
		"uppercase":       "TenantEtcd",
		"trailing-hyphen": "tenant-",
		"too-long":        strings.Repeat("a", 57),
	}
	for name, clusterName := range rejected {
		t.Run(name, func(t *testing.T) {
			c := validCluster(clusterName)
			if err := k8s.Create(ctx, c); err == nil {
				_ = k8s.Delete(ctx, c)
				t.Fatalf("apiserver accepted invalid cluster name %q; expected rejection", clusterName)
			}
		})
	}

	accepted := []string{"bc4ktaf-etcd", "a", strings.Repeat("a", 56), "t1-etcd-9"}
	for _, clusterName := range accepted {
		c := validCluster(clusterName)
		if err := k8s.Create(ctx, c); err != nil {
			t.Fatalf("apiserver rejected valid cluster name %q: %v", clusterName, err)
		}
		_ = k8s.Delete(ctx, c)
	}
}
