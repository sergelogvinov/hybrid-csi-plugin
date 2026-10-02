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

package provisioner

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Provisioning phases of the hybrid_provision_phase_total metric.
const (
	// phaseBackend is the choice of the backend StorageClass, result success or error (none fits the node).
	phaseBackend = "backend"
	// phaseHelper is the creation of the helper, result success or error.
	phaseHelper = "helper"
	// phaseMove is the move of the volume and the bind of the claim, result success or error.
	phaseMove = "move"
	// phaseCleanup is the deletion of the helper and the release of the claim, result success or error.
	phaseCleanup = "cleanup"
	// phaseReschedule is a reschedule of the claim, the result is the reason: one of reschedule* constants.
	phaseReschedule = "reschedule"
)

// Results of the hybrid_provision_phase_total metric.
const (
	resultSuccess = "success"
	resultError   = "error"

	rescheduleNoBackend    = "no-backend"
	rescheduleNodeAffinity = "node-affinity"
	rescheduleTimeout      = "helper-timeout"
	rescheduleRejected     = "backend-rejected"
)

type metrics struct {
	phases    *prometheus.CounterVec
	helperAge prometheus.Histogram
}

func newMetrics() *metrics {
	return &metrics{
		phases: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hybrid_provision_phase_total",
			Help: "Number of provisioning phases by result.",
		}, []string{"phase", "result"}),
		helperAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "hybrid_helper_age_seconds",
			Help:    "Age of helper persistent volume claims when they are deleted.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 13), // 1s .. ~68m
		}),
	}
}

func (m *metrics) phase(phase string, err error) {
	result := resultSuccess
	if err != nil {
		result = resultError
	}

	m.phases.WithLabelValues(phase, result).Inc()
}

// Collectors returns the metrics of the provisioner.
func (p *HybridProvisioner) Collectors() []prometheus.Collector {
	return []prometheus.Collector{p.metrics.phases, p.metrics.helperAge}
}
