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
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Nothing is created on a node without a backend: the claim is rescheduled without a write, and it is not counted.
func TestRescheduleNoBackend(t *testing.T) {
	env := newTestEnv(t,
		testNode(nodeB, nil),
		testCSIDriver(csiA),
		testCSINode(nodeB, csiB),
		testStorageClass(backendA, csiA),
		testHybridStorageClass(backendA),
		testPVC(testClaimName),
	)

	for range testMaxReschedules + 1 {
		if _, state, err := env.provision(testClaimName, nodeB); err == nil || state != controller.ProvisioningReschedule {
			t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningReschedule)
		}

		requireEvent(t, env.events(), ReasonRescheduled, "no backend storage class for node "+nodeB)
	}

	if env.writes != 0 {
		t.Errorf("Provision() made %d writes, want 0", env.writes)
	}

	if got := testutil.ToFloat64(env.prov.metrics.phases.WithLabelValues(phaseReschedule, rescheduleNoBackend)); got != testMaxReschedules+1 {
		t.Errorf("reschedule metric = %v, want %d", got, testMaxReschedules+1)
	}
}

// Every counted cause reschedules the claim the same way, until the limit.
func TestRescheduleCauses(t *testing.T) {
	tests := []struct {
		name   string
		result string
		giveUp string
		text   string
		// helper changes the pending helper on node-a and returns the objects of the cause.
		helper func(helper *corev1.PersistentVolumeClaim) []runtime.Object
	}{
		{
			name: "helper timeout", result: rescheduleTimeout, giveUp: ReasonHelperTimeout, text: "is not provisioned in",
			helper: func(helper *corev1.PersistentVolumeClaim) []runtime.Object {
				helper.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * testHelperTimeout))

				return []runtime.Object{helper}
			},
		},
		{
			name: "backend rejected", result: rescheduleRejected, giveUp: ReasonBackendRejected, text: "is rejected by the backend on node " + nodeA,
			helper: func(helper *corev1.PersistentVolumeClaim) []runtime.Object {
				helper.Annotations[annotationSelectedNode] = ""

				return []runtime.Object{helper}
			},
		},
		{
			name: "volume node mismatch", result: rescheduleNodeAffinity, giveUp: ReasonVolumeNodeMismatch, text: "does not fit node " + nodeA,
			helper: func(helper *corev1.PersistentVolumeClaim) []runtime.Object {
				pv := testBackendPV(helper, testStorageClass(backendA, csiA))
				pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{nodeB}
				helper.Spec.VolumeName = pv.Name

				return []runtime.Object{helper, pv}
			},
		},
	}

	newEnv := func(t *testing.T, reschedules int, cause func(helper *corev1.PersistentVolumeClaim) []runtime.Object) (*testEnv, *corev1.PersistentVolumeClaim) {
		t.Helper()

		claim := testPVC(testClaimName)
		claim.Finalizers = []string{FinalizerProvisioning}
		claim.Annotations = map[string]string{
			AnnotationBackendClass: backendA,
			AnnotationHelper:       HelperName(claim),
			AnnotationReschedules:  strconv.Itoa(reschedules),
			annotationSelectedNode: nodeA,
		}

		helper := buildHelperPVC(claim, testStorageClass(backendA, csiA), nodeA)
		helper.UID = "uid-helper"
		helper.CreationTimestamp = metav1.Now()

		return newTestEnv(t, testCluster(append(cause(helper), claim)...)...), claim
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, claim := newEnv(t, testMaxReschedules-1, tt.helper)

			if _, state, err := env.provision(testClaimName, nodeA); err == nil || state != controller.ProvisioningReschedule {
				t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningReschedule)
			}

			events := env.events()
			requireEvent(t, events, ReasonRescheduled, fmt.Sprintf("(%d of %d): ", testMaxReschedules, testMaxReschedules))
			requireEvent(t, events, ReasonRescheduled, tt.text)

			if _, err := env.getPVC(HelperName(claim)); !apierrors.IsNotFound(err) {
				t.Errorf("helper PVC is not deleted: %v", err)
			}

			u, err := env.getPVC(testClaimName)
			if err != nil {
				t.Fatalf("failed to get user PVC: %v", err)
			}

			if got := u.Annotations[AnnotationReschedules]; got != strconv.Itoa(testMaxReschedules) {
				t.Errorf("reschedules = %q, want %d", got, testMaxReschedules)
			}

			for _, key := range []string{annotationSelectedNode, AnnotationBackendClass} {
				if v, ok := u.Annotations[key]; ok {
					t.Errorf("user PVC annotation %s = %q, want none", key, v)
				}
			}

			if got := testutil.ToFloat64(env.prov.metrics.phases.WithLabelValues(phaseReschedule, tt.result)); got != 1 {
				t.Errorf("reschedule metric = %v, want 1", got)
			}
		})

		t.Run(tt.name+", limit", func(t *testing.T) {
			env, claim := newEnv(t, testMaxReschedules, tt.helper)

			if _, state, err := env.provision(testClaimName, nodeA); err == nil || state != controller.ProvisioningInBackground {
				t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningInBackground)
			}

			requireEvent(t, env.events(), tt.giveUp, tt.text)

			if _, err := env.getPVC(HelperName(claim)); err != nil {
				t.Errorf("helper PVC is deleted: %v", err)
			}
		})
	}
}
