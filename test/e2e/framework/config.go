//go:build e2e

/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package framework provides the plumbing (clients, waiters, object builders)
// shared by the hybrid-csi-plugin end-to-end tests.
package framework

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// Config holds the e2e suite configuration, read from environment variables.
//
// The suite never deploys the hybrid controller, the backends or the
// StorageClasses itself: the target cluster (selected via KUBECONFIG) is
// expected to already have all of them.
type Config struct {
	// Kubeconfig is the path to the kubeconfig of the cluster under test.
	// Empty means "use client-go's default loading rules" (KUBECONFIG env,
	// then ~/.kube/config).
	Kubeconfig string

	// StorageClass is the hybrid StorageClass under test. Its backends are
	// read from its parameters.storageClasses.
	StorageClass string

	// Replicas is the StatefulSet size. Replicas are spread across nodes
	// by pod anti-affinity, so the cluster needs at least that many
	// schedulable nodes.
	Replicas int32

	// NamespacePrefix namespaces created by the suite are named
	// "<prefix>-<random>" and removed on cleanup.
	NamespacePrefix string

	// Timeout is the default per-wait timeout (pod ready, PVC bound, PV
	// gone, ...).
	Timeout time.Duration

	// SweepAge is the minimum age of a leftover namespace before it is
	// swept. Younger namespaces may belong to a run that is still going:
	// go test runs scenario packages in parallel, and several runs may
	// share one cluster.
	SweepAge time.Duration
}

// LoadConfig builds a Config from environment variables.
func LoadConfig() Config {
	return Config{
		Kubeconfig:      os.Getenv("KUBECONFIG"),
		StorageClass:    getEnvDefault("E2E_STORAGECLASS", "hybrid"),
		Replicas:        int32(getEnvIntDefault("E2E_REPLICAS", 2)),
		NamespacePrefix: getEnvDefault("E2E_NAMESPACE_PREFIX", "e2e"),
		Timeout:         getEnvDurationDefault("E2E_TIMEOUT", 5*time.Minute),
		SweepAge:        getEnvDurationDefault("E2E_SWEEP_AGE", 2*time.Hour),
	}
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func getEnvIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(v)
		if err == nil && i > 0 {
			return i
		}

		log.Printf("e2e: invalid %s=%q, using default %d", key, v, def)
	}

	return def
}

func getEnvDurationDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
		}

		if err == nil {
			err = fmt.Errorf("must be positive")
		}

		log.Printf("e2e: invalid %s=%q (%v), using default %s", key, v, err, def)
	}

	return def
}
