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
	"testing"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-helpers/storage/volume"
)

const (
	nodeA    = "node-a"
	nodeB    = "node-b"
	backendA = "backend-a"
	backendB = "backend-b"
)

// testCluster has two nodes, served by two different CSI backends.
func testCluster(objs ...runtime.Object) []runtime.Object {
	return append([]runtime.Object{
		testNode(nodeA, nil),
		testNode(nodeB, nil),
		testCSIDriver(csiA),
		testCSIDriver(csiB),
		testCSINode(nodeA, csiA),
		testCSINode(nodeB, csiB),
		testStorageClass(backendA, csiA),
		testStorageClass(backendB, csiB),
		testHybridStorageClass(testHybridClass, backendA, backendB),
	}, objs...)
}

func TestProvisionGuards(t *testing.T) {
	env := newTestEnv(t, methodDefault, testCluster(testPVC("data"))...)
	base := env.provisionOptions("data", nodeA)

	tests := []struct {
		name  string
		opts  func(o controller.ProvisionOptions) controller.ProvisionOptions
		state controller.ProvisioningState
	}{
		{
			name: "no storage class",
			opts: func(o controller.ProvisionOptions) controller.ProvisionOptions {
				o.StorageClass = nil

				return o
			},
			state: controller.ProvisioningFinished,
		},
		{
			name: "no storageClasses parameter",
			opts: func(o controller.ProvisionOptions) controller.ProvisionOptions {
				o.StorageClass = testStorageClass(testHybridClass, DriverName)

				return o
			},
			state: controller.ProvisioningFinished,
		},
		{
			name: "no selected node",
			opts: func(o controller.ProvisionOptions) controller.ProvisionOptions {
				o.SelectedNode = nil

				return o
			},
			state: controller.ProvisioningFinished,
		},
		{
			name: "no backend for the node",
			opts: func(o controller.ProvisionOptions) controller.ProvisionOptions {
				o.SelectedNode = testNode("node-c", nil)

				return o
			},
			state: controller.ProvisioningReschedule,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv, state, err := env.prov.Provision(t.Context(), tt.opts(base))
			if err == nil || pv != nil {
				t.Fatalf("Provision() = %v, %v; want error", pv, err)
			}

			if state != tt.state {
				t.Errorf("Provision() state = %v, want %v", state, tt.state)
			}
		})
	}
}

func TestProvision(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		node    string
		backend string
		driver  string
	}{
		{name: "annotation method, first backend", method: methodAnnotation, node: nodeA, backend: backendA, driver: csiA},
		{name: "annotation method, second backend", method: methodAnnotation, node: nodeB, backend: backendB, driver: csiB},
		{name: "auto method", method: methodDefault, node: nodeB, backend: backendB, driver: csiB},
		{name: "pod method", method: methodPod, node: nodeA, backend: backendA, driver: csiA},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.method, testCluster(testPVC("data"))...)
			env.simulateBackend()

			opts := env.provisionOptions("data", tt.node)

			pv, state, err := env.provisionUntilDone(opts, 3)
			if err != nil {
				t.Fatalf("Provision() error = %v", err)
			}

			if state != controller.ProvisioningFinished {
				t.Fatalf("Provision() state = %v, want %v", state, controller.ProvisioningFinished)
			}

			// Returned PV: the backend volume, ready to be created by the library.
			if pv.ResourceVersion != "" {
				t.Errorf("returned PV has resourceVersion %q", pv.ResourceVersion)
			}

			if pv.Spec.StorageClassName != tt.backend {
				t.Errorf("PV storage class = %s, want %s", pv.Spec.StorageClassName, tt.backend)
			}

			if pv.Spec.CSI.Driver != tt.driver {
				t.Errorf("PV driver = %s, want %s", pv.Spec.CSI.Driver, tt.driver)
			}

			// Helper PVC is gone.
			if _, err = env.getPVC(opts.PVName); !apierrors.IsNotFound(err) {
				t.Errorf("helper PVC still exists, err = %v", err)
			}

			// Helper pod is gone.
			if _, err = env.client.CoreV1().Pods(testNamespace).Get(t.Context(), helperPodName(opts), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Errorf("helper pod still exists, err = %v", err)
			}

			// User PVC points at the PV.
			pvc, err := env.getPVC("data")
			if err != nil {
				t.Fatalf("failed to get user PVC: %v", err)
			}

			if pvc.Spec.VolumeName != pv.Name {
				t.Errorf("user PVC volumeName = %q, want %q", pvc.Spec.VolumeName, pv.Name)
			}

			for k, v := range map[string]string{
				volume.AnnBindCompleted:     "yes",
				volume.AnnBoundByController: "yes",
				annStorageProvisioner:       tt.driver,
			} {
				if pvc.Annotations[k] != v {
					t.Errorf("user PVC annotation %s = %q, want %q", k, pvc.Annotations[k], v)
				}
			}

			// Backend PV is bound to the user PVC with the backend reclaim policy.
			pv, err = env.getPV(pv.Name)
			if err != nil {
				t.Fatalf("failed to get PV: %v", err)
			}

			if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID || pv.Spec.ClaimRef.Name != pvc.Name {
				t.Errorf("PV claimRef = %+v, want user PVC %s", pv.Spec.ClaimRef, pvc.UID)
			}

			if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				t.Errorf("PV reclaim policy = %s, want Delete", pv.Spec.PersistentVolumeReclaimPolicy)
			}
		})
	}
}

// TestProvisionRetainBackend documents v0.x behavior: with a Retain backend class the PV
// is left Retain with no claimRef, the PV controller binds it by U.spec.volumeName.
func TestProvisionRetainBackend(t *testing.T) {
	retain := testStorageClass("backend-retain", csiA)
	policy := corev1.PersistentVolumeReclaimRetain
	retain.ReclaimPolicy = &policy

	env := newTestEnv(t, methodAnnotation,
		testNode(nodeA, nil),
		testCSIDriver(csiA),
		testCSINode(nodeA, csiA),
		retain,
		testHybridStorageClass(testHybridClass, "backend-retain"),
		testPVC("data"),
	)
	env.simulateBackend()

	pv, _, err := env.provisionUntilDone(env.provisionOptions("data", nodeA), 3)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	pv, err = env.getPV(pv.Name)
	if err != nil {
		t.Fatalf("failed to get PV: %v", err)
	}

	if pv.Spec.ClaimRef != nil {
		t.Errorf("PV claimRef = %+v, want nil", pv.Spec.ClaimRef)
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("PV reclaim policy = %s, want Retain", pv.Spec.PersistentVolumeReclaimPolicy)
	}
}

// TestProvisionBindTimeout documents v0.x behavior: the helper PVC is left behind
// when the backend does not bind it in time.
func TestProvisionBindTimeout(t *testing.T) {
	env := newTestEnv(t, methodAnnotation, testCluster(testPVC("data"))...)
	env.prov.bindTimeout = 100 * time.Millisecond

	opts := env.provisionOptions("data", nodeA)

	_, state, err := env.prov.Provision(t.Context(), opts)
	if err == nil {
		t.Fatalf("Provision() expected timeout error")
	}

	if state != controller.ProvisioningFinished {
		t.Errorf("Provision() state = %v, want %v", state, controller.ProvisioningFinished)
	}

	helper, err := env.getPVC(opts.PVName)
	if err != nil {
		t.Fatalf("helper PVC not found: %v", err)
	}

	if *helper.Spec.StorageClassName != backendA {
		t.Errorf("helper storage class = %s, want backend-a", *helper.Spec.StorageClassName)
	}

	if helper.Annotations[annSelectedNode] != nodeA {
		t.Errorf("helper selected node = %q, want node-a", helper.Annotations[annSelectedNode])
	}
}

func TestBuildHelperPVC(t *testing.T) {
	env := newTestEnv(t, methodDefault, testCluster(testPVC("data"))...)
	opts := env.provisionOptions("data", nodeB)
	sc := testStorageClass(backendB, csiB)

	helper := buildHelperPVC(opts, sc, true)

	if helper.Name != opts.PVName || helper.Namespace != testNamespace {
		t.Errorf("helper = %s/%s, want %s/%s", helper.Namespace, helper.Name, testNamespace, opts.PVName)
	}

	if *helper.Spec.StorageClassName != sc.Name {
		t.Errorf("helper storage class = %s, want %s", *helper.Spec.StorageClassName, sc.Name)
	}

	if !helper.Spec.Resources.Requests.Storage().Equal(*opts.PVC.Spec.Resources.Requests.Storage()) {
		t.Errorf("helper request = %v, want %v", helper.Spec.Resources.Requests.Storage(), opts.PVC.Spec.Resources.Requests.Storage())
	}

	for k, v := range map[string]string{
		annStorageProvisioner:     csiB,
		annBetaStorageProvisioner: csiB,
		annSelectedNode:           nodeB,
	} {
		if helper.Annotations[k] != v {
			t.Errorf("helper annotation %s = %q, want %q", k, helper.Annotations[k], v)
		}
	}

	if helper = buildHelperPVC(opts, sc, false); len(helper.Annotations) != 0 {
		t.Errorf("helper for pod method has annotations %v", helper.Annotations)
	}

	pod := buildHelperPod(opts, helper)
	if pod.Spec.NodeSelector[corev1.LabelHostname] != nodeB {
		t.Errorf("helper pod node selector = %v", pod.Spec.NodeSelector)
	}

	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != helper.Name {
		t.Errorf("helper pod claim = %s, want %s", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, helper.Name)
	}
}

// TestProvisionSpread provisions several claims on different nodes, like a StatefulSet.
func TestProvisionSpread(t *testing.T) {
	env := newTestEnv(t, methodAnnotation, testCluster(
		testPVC("data-0"),
		testPVC("data-1"),
	)...)
	env.simulateBackend()

	for name, node := range map[string]string{"data-0": nodeA, "data-1": nodeB} {
		pv, _, err := env.provisionUntilDone(env.provisionOptions(name, node), 3)
		if err != nil {
			t.Fatalf("Provision(%s) error = %v", name, err)
		}

		want := map[string]string{nodeA: backendA, nodeB: backendB}[node]
		if pv.Spec.StorageClassName != want {
			t.Errorf("PV for %s storage class = %s, want %s", name, pv.Spec.StorageClassName, want)
		}

		pvc, err := env.getPVC(name)
		if err != nil {
			t.Fatalf("failed to get user PVC: %v", err)
		}

		if pvc.Spec.VolumeName != pv.Name {
			t.Errorf("user PVC %s volumeName = %q, want %q", name, pvc.Spec.VolumeName, pv.Name)
		}
	}
}
