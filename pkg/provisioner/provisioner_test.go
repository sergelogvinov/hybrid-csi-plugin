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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/utils/ptr"
)

const (
	foreignUID = "someone-else"

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
		testHybridStorageClass(backendA, backendB),
	}, objs...)
}

// testLegacyHelper is a helper PVC of the user PVC created by v0.x.
func testLegacyHelper(claim *corev1.PersistentVolumeClaim, class string) *corev1.PersistentVolumeClaim {
	helper := claim.DeepCopy()
	helper.Name = HelperName(claim)
	helper.UID = types.UID("uid-" + helper.Name)
	helper.Spec.StorageClassName = &class
	helper.Annotations = map[string]string{annotationSelectedNode: nodeA}

	return helper
}

// requireProvisioned checks the converged state after provisioning of the user PVC "data": the claim is bound
// to the volume, the volume is moved to it with the hybrid metadata and is the only PV in the cluster.
func requireProvisioned(t *testing.T, env *testEnv, backend string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	t.Helper()

	const pvcName = "data"

	pvc, err := env.getPVC(pvcName)
	if err != nil {
		t.Fatalf("failed to get user PVC: %v", err)
	}

	if pvc.Spec.VolumeName == "" {
		t.Fatalf("user PVC is not bound")
	}

	pv, err := env.getPV(pvc.Spec.VolumeName)
	if err != nil {
		t.Fatalf("failed to get PV: %v", err)
	}

	if pv.Spec.StorageClassName != backend {
		t.Errorf("PV storage class = %s, want %s", pv.Spec.StorageClassName, backend)
	}

	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID || pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.Namespace != pvc.Namespace {
		t.Errorf("PV claimRef = %+v, want user PVC %s", pv.Spec.ClaimRef, pvc.UID)
	}

	if pv.Labels[LabelManaged] != "true" {
		t.Errorf("PV label %s = %q, want true", LabelManaged, pv.Labels[LabelManaged])
	}

	for k, v := range map[string]string{
		AnnotationClaim:        testNamespace + "/" + pvcName,
		AnnotationStorageClass: testHybridClass,
	} {
		if pv.Annotations[k] != v {
			t.Errorf("PV annotation %s = %q, want %q", k, pv.Annotations[k], v)
		}
	}

	for k, v := range map[string]string{
		volume.AnnBindCompleted:      valueYes,
		volume.AnnBoundByController:  valueYes,
		annotationStorageProvisioner: pv.Spec.CSI.Driver,
		AnnotationBackendClass:       backend,
	} {
		if pvc.Annotations[k] != v {
			t.Errorf("user PVC annotation %s = %q, want %q", k, pvc.Annotations[k], v)
		}
	}

	if pvs := env.listPVs(); len(pvs) != 1 {
		t.Errorf("cluster has %d PVs, want 1", len(pvs))
	}

	return pvc, pv
}

func TestProvisionGuards(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
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
			name: "claim is being deleted",
			opts: func(o controller.ProvisionOptions) controller.ProvisionOptions {
				o.PVC = o.PVC.DeepCopy()
				o.PVC.DeletionTimestamp = &metav1.Time{}

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

	if env.writes != 0 {
		t.Errorf("guards made %d writes, want 0", env.writes)
	}
}

func TestProvision(t *testing.T) {
	tests := []struct {
		name    string
		node    string
		backend string
		driver  string
	}{
		{name: "first backend", node: nodeA, backend: backendA, driver: csiA},
		{name: "second backend", node: nodeB, backend: backendB, driver: csiB},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, testCluster(testPVC("data"))...)
			env.backend = true

			pv, state, err := env.provisionUntilDone("data", tt.node, 3)
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

			if pv.Spec.CSI.Driver != tt.driver {
				t.Errorf("PV driver = %s, want %s", pv.Spec.CSI.Driver, tt.driver)
			}

			pvc, stored := requireProvisioned(t, env, tt.backend)

			if pv.Name != stored.Name {
				t.Errorf("returned PV = %s, want %s", pv.Name, stored.Name)
			}

			// The helper is gone and the claim is released.
			if _, err = env.getPVC(HelperName(pvc)); !apierrors.IsNotFound(err) {
				t.Errorf("helper PVC still exists, err = %v", err)
			}

			if slices.Contains(pvc.Finalizers, FinalizerProvisioning) {
				t.Errorf("user PVC still has finalizer %s", FinalizerProvisioning)
			}

			pods, err := env.client.CoreV1().Pods("").List(t.Context(), metav1.ListOptions{})
			if err != nil || len(pods.Items) != 0 {
				t.Errorf("provisioner created pods: %v, %v", pods, err)
			}
		})
	}
}

// TestProvisionDataSource: the data source of the claim is passed to the helper, a restore is never an empty volume.
func TestProvisionDataSource(t *testing.T) {
	claim := testPVC("data")
	claim.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: new("snapshot.storage.k8s.io"),
		Kind:     "VolumeSnapshot",
		Name:     "snap",
	}
	claim.Spec.DataSourceRef = &corev1.TypedObjectReference{
		APIGroup: new("snapshot.storage.k8s.io"),
		Kind:     "VolumeSnapshot",
		Name:     "snap",
	}
	claim.Spec.VolumeAttributesClassName = new("fast")

	env := newTestEnv(t, testCluster(claim)...)

	requirePending(t, env)

	helper, err := env.getPVC(HelperName(claim))
	if err != nil {
		t.Fatalf("failed to get helper PVC: %v", err)
	}

	if !apiequality.Semantic.DeepEqual(helper.Spec.DataSource, claim.Spec.DataSource) {
		t.Errorf("helper dataSource = %+v, want %+v", helper.Spec.DataSource, claim.Spec.DataSource)
	}

	if !apiequality.Semantic.DeepEqual(helper.Spec.DataSourceRef, claim.Spec.DataSourceRef) {
		t.Errorf("helper dataSourceRef = %+v, want %+v", helper.Spec.DataSourceRef, claim.Spec.DataSourceRef)
	}

	if got := ptr.Deref(helper.Spec.VolumeAttributesClassName, ""); got != "fast" {
		t.Errorf("helper volumeAttributesClassName = %q, want %q", got, "fast")
	}
}

// TestProvisionHelper checks the claim and the helper while the backend is provisioning.
func TestProvisionHelper(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	for range 3 {
		_, state, err := env.provision("data", nodeB)
		if state != controller.ProvisioningInBackground || err == nil {
			t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningInBackground)
		}
	}

	// The claim: finalizer, pinned backend and helper name in one write, then the helper.
	if env.writes != 2 {
		t.Errorf("provisioner made %d writes, want 2", env.writes)
	}

	pvc, err := env.getPVC("data")
	if err != nil {
		t.Fatalf("failed to get user PVC: %v", err)
	}

	if !slices.Contains(pvc.Finalizers, FinalizerProvisioning) {
		t.Errorf("user PVC finalizers = %v, want %s", pvc.Finalizers, FinalizerProvisioning)
	}

	for k, v := range map[string]string{AnnotationBackendClass: backendB, AnnotationHelper: HelperName(pvc)} {
		if pvc.Annotations[k] != v {
			t.Errorf("user PVC annotation %s = %q, want %q", k, pvc.Annotations[k], v)
		}
	}

	helper, err := env.getPVC(HelperName(pvc))
	if err != nil {
		t.Fatalf("helper PVC not found: %v", err)
	}

	if *helper.Spec.StorageClassName != backendB {
		t.Errorf("helper storage class = %s, want %s", *helper.Spec.StorageClassName, backendB)
	}

	if !helper.Spec.Resources.Requests.Storage().Equal(*pvc.Spec.Resources.Requests.Storage()) {
		t.Errorf("helper request = %v, want %v", helper.Spec.Resources.Requests.Storage(), pvc.Spec.Resources.Requests.Storage())
	}

	if helper.Labels[LabelRole] != LabelRoleHelper {
		t.Errorf("helper label %s = %q, want %q", LabelRole, helper.Labels[LabelRole], LabelRoleHelper)
	}

	for k, v := range map[string]string{
		AnnotationOwnerUID:               string(pvc.UID),
		annotationSelectedNode:           nodeB,
		annotationStorageProvisioner:     csiB,
		annotationBetaStorageProvisioner: csiB,
	} {
		if helper.Annotations[k] != v {
			t.Errorf("helper annotation %s = %q, want %q", k, helper.Annotations[k], v)
		}
	}

	if !slices.Equal(helper.Finalizers, []string{FinalizerHelper}) {
		t.Errorf("helper finalizers = %v, want %s", helper.Finalizers, FinalizerHelper)
	}

	owner := metav1.GetControllerOf(helper)
	if owner == nil || owner.UID != pvc.UID || owner.Kind != KindPersistentVolumeClaim || *owner.BlockOwnerDeletion {
		t.Errorf("helper controller = %+v, want user PVC %s without blockOwnerDeletion", owner, pvc.UID)
	}
}

// TestProvisionCrash: a write of the provisioner fails at every point of the happy path,
// and the next passes converge to the same state as without the fault.
func TestProvisionCrash(t *testing.T) {
	// The happy path, to count the writes.
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.backend = true

	if _, _, err := env.provisionUntilDone("data", nodeA, 3); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	writes := env.writes
	if writes != 7 {
		t.Fatalf("happy path made %d writes, want 7 (claim, helper, move, bind, helper finalizer, helper delete, claim finalizer)", writes)
	}

	for failAt := 1; failAt <= writes; failAt++ {
		t.Run(map[int]string{
			1: "prepare claim",
			2: "create helper",
			3: "move volume",
			4: "bind claim",
			5: "helper finalizer",
			6: "delete helper",
			7: "claim finalizer",
		}[failAt], func(t *testing.T) {
			env := newTestEnv(t, testCluster(testPVC("data"))...)
			env.backend = true
			env.failAt = failAt

			pv, state, err := env.provisionUntilDone("data", nodeA, 5)
			if err != nil || state != controller.ProvisioningFinished {
				t.Fatalf("Provision() = %v, %v", state, err)
			}

			pvc, stored := requireProvisioned(t, env, backendA)

			if pv != nil && pv.Name != stored.Name {
				t.Errorf("returned PV = %s, want %s", pv.Name, stored.Name)
			}

			_, err = env.getPVC(HelperName(pvc))
			helperLeft := err == nil
			claimLeft := slices.Contains(pvc.Finalizers, FinalizerProvisioning)

			// After the bind the claim is bound and the library stops calling Provision(),
			// the leftovers are cleaned up by the lifecycle controller.
			// A failed finalizer patch (5, 7) is retried at once with the current object,
			// only a failed delete of the helper (6) leaves the helper and the claim finalizer behind.
			wantHelper := failAt == 6
			wantClaim := failAt == 6

			if helperLeft != wantHelper {
				t.Errorf("helper PVC left = %v, want %v", helperLeft, wantHelper)
			}

			if claimLeft != wantClaim {
				t.Errorf("user PVC finalizer left = %v, want %v", claimLeft, wantClaim)
			}
		})
	}
}

func TestProvisionReclaimPolicy(t *testing.T) {
	tests := []struct {
		backend corev1.PersistentVolumeReclaimPolicy
		hybrid  *corev1.PersistentVolumeReclaimPolicy
		want    corev1.PersistentVolumeReclaimPolicy
	}{
		{backend: corev1.PersistentVolumeReclaimDelete, hybrid: new(corev1.PersistentVolumeReclaimDelete), want: corev1.PersistentVolumeReclaimDelete},
		{backend: corev1.PersistentVolumeReclaimRetain, hybrid: new(corev1.PersistentVolumeReclaimDelete), want: corev1.PersistentVolumeReclaimDelete},
		{backend: corev1.PersistentVolumeReclaimDelete, hybrid: new(corev1.PersistentVolumeReclaimRetain), want: corev1.PersistentVolumeReclaimRetain},
		{backend: corev1.PersistentVolumeReclaimRetain, hybrid: new(corev1.PersistentVolumeReclaimRetain), want: corev1.PersistentVolumeReclaimRetain},
		{backend: corev1.PersistentVolumeReclaimRetain, hybrid: nil, want: corev1.PersistentVolumeReclaimDelete},
	}

	for _, tt := range tests {
		name := "backend " + string(tt.backend) + ", hybrid "
		if tt.hybrid != nil {
			name += string(*tt.hybrid)
		}

		t.Run(name, func(t *testing.T) {
			backend := testStorageClass(backendA, csiA)
			backend.ReclaimPolicy = &tt.backend

			hybrid := testHybridStorageClass(backendA)
			hybrid.ReclaimPolicy = tt.hybrid

			env := newTestEnv(t,
				testNode(nodeA, nil),
				testCSIDriver(csiA),
				testCSINode(nodeA, csiA),
				backend,
				hybrid,
				testPVC("data"),
			)
			env.backend = true

			if _, _, err := env.provisionUntilDone("data", nodeA, 3); err != nil {
				t.Fatalf("Provision() error = %v", err)
			}

			_, pv := requireProvisioned(t, env, backendA)
			if pv.Spec.PersistentVolumeReclaimPolicy != tt.want {
				t.Errorf("PV reclaim policy = %s, want %s", pv.Spec.PersistentVolumeReclaimPolicy, tt.want)
			}
		})
	}
}

// TestProvisionPinnedBackend checks that the backend pinned on the claim is reused while it fits the node.
func TestProvisionPinnedBackend(t *testing.T) {
	tests := []struct {
		name    string
		pinned  string
		drivers []string
		want    string
	}{
		{name: "pinned backend fits", pinned: backendB, drivers: []string{csiA, csiB}, want: backendB},
		{name: "pinned backend does not fit", pinned: backendB, drivers: []string{csiA}, want: backendA},
		{name: "pinned backend is not in the list", pinned: "backend-c", drivers: []string{csiA, csiB}, want: backendA},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := testPVC("data")
			claim.Annotations = map[string]string{AnnotationBackendClass: tt.pinned}

			env := newTestEnv(t,
				testNode(nodeA, nil),
				testCSIDriver(csiA),
				testCSIDriver(csiB),
				testCSINode(nodeA, tt.drivers...),
				testStorageClass(backendA, csiA),
				testStorageClass(backendB, csiB),
				testStorageClass("backend-c", csiB),
				testHybridStorageClass(backendA, backendB),
				claim,
			)
			env.backend = true

			if _, _, err := env.provisionUntilDone("data", nodeA, 3); err != nil {
				t.Fatalf("Provision() error = %v", err)
			}

			requireProvisioned(t, env, tt.want)
		})
	}
}

// TestProvisionHelperNotOwned checks that a PVC with the helper name that is not ours is never touched.
func TestProvisionHelperNotOwned(t *testing.T) {
	claim := testPVC("data")

	foreign := testLegacyHelper(claim, backendA)
	foreign.Annotations[AnnotationOwnerUID] = foreignUID

	env := newTestEnv(t, testCluster(claim, foreign)...)

	_, state, err := env.provision("data", nodeA)
	if err == nil || state != controller.ProvisioningFinished {
		t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningFinished)
	}

	if env.writes != 0 {
		t.Errorf("provisioner made %d writes, want 0", env.writes)
	}
}

// TestProvisionLegacyHelper checks the adoption of helper PVCs created by v0.x.
func TestProvisionLegacyHelper(t *testing.T) {
	claim := testPVC("data")

	t.Run("matching helper is adopted", func(t *testing.T) {
		env := newTestEnv(t, testCluster(claim, testLegacyHelper(claim, backendB))...)

		_, state, err := env.provision("data", nodeA)
		if state != controller.ProvisioningInBackground {
			t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningInBackground)
		}

		helper, err := env.getPVC(HelperName(claim))
		if err != nil {
			t.Fatalf("helper PVC not found: %v", err)
		}

		if helper.Annotations[AnnotationOwnerUID] != string(claim.UID) || helper.Labels[LabelRole] != LabelRoleHelper ||
			!slices.Contains(helper.Finalizers, FinalizerHelper) || metav1.GetControllerOf(helper) == nil {
			t.Errorf("helper PVC is not adopted: %+v", helper.ObjectMeta)
		}

		// The volume is provisioned by the adopted helper, on its backend.
		env.backend = true

		if _, _, err = env.provisionUntilDone("data", nodeA, 3); err != nil {
			t.Fatalf("Provision() error = %v", err)
		}

		requireProvisioned(t, env, backendB)
	})

	t.Run("helper of another size is not adopted", func(t *testing.T) {
		helper := testLegacyHelper(claim, backendA)
		helper.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")

		env := newTestEnv(t, testCluster(claim, helper)...)

		_, state, err := env.provision("data", nodeA)
		if err == nil || state != controller.ProvisioningFinished {
			t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningFinished)
		}

		if env.writes != 0 {
			t.Errorf("provisioner made %d writes, want 0", env.writes)
		}
	})
}

// TestProvisionVolumeNodeAffinity: the backend ignored selected-node, the volume is discarded and the claim rescheduled.
func TestProvisionVolumeNodeAffinity(t *testing.T) {
	backend := testStorageClass(backendA, csiA)
	backend.ReclaimPolicy = new(corev1.PersistentVolumeReclaimRetain)

	env := newTestEnv(t,
		testNode(nodeA, nil),
		testCSIDriver(csiA),
		testCSINode(nodeA, csiA),
		backend,
		testHybridStorageClass(backendA),
		testPVC("data"),
	)
	env.volumeHook = func(pv *corev1.PersistentVolume) {
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{nodeB}
	}
	env.selectNode()

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	env.bindHelpers()

	_, state, err := env.provision("data", nodeA)
	if err == nil || state != controller.ProvisioningReschedule {
		t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningReschedule)
	}

	pvc, err := env.getPVC("data")
	if err != nil {
		t.Fatalf("failed to get user PVC: %v", err)
	}

	if _, ok := pvc.Annotations[AnnotationBackendClass]; ok || pvc.Spec.VolumeName != "" {
		t.Errorf("user PVC is pinned or bound: %v, %q", pvc.Annotations, pvc.Spec.VolumeName)
	}

	// The selected node is removed here: the library removes it with a stale claim and fails.
	if _, ok := pvc.Annotations[annotationSelectedNode]; ok {
		t.Errorf("user PVC selected node is not removed")
	}

	requireEvent(t, env.events(), ReasonRescheduled, "does not fit node")

	if got := pvc.Annotations[AnnotationReschedules]; got != "1" {
		t.Errorf("reschedules = %q, want 1", got)
	}

	if _, err = env.getPVC(HelperName(pvc)); !apierrors.IsNotFound(err) {
		t.Errorf("helper PVC still exists, err = %v", err)
	}

	// The fresh volume is deleted by the backend, even with the Retain class.
	pvs := env.listPVs()
	if len(pvs) != 1 || pvs[0].Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PVs = %+v, want one with reclaim policy Delete", pvs)
	}
}

// TestProvisionSelectedNodeChanged: the pending helper of the old node is replaced.
func TestProvisionSelectedNodeChanged(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	_, state, err := env.provision("data", nodeB)
	if err == nil || state != controller.ProvisioningInBackground {
		t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningInBackground)
	}

	if _, err = env.getPVC(HelperName(testPVC("data"))); !apierrors.IsNotFound(err) {
		t.Fatalf("helper PVC of the old node still exists, err = %v", err)
	}

	env.backend = true

	if _, _, err = env.provisionUntilDone("data", nodeB, 3); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	requireProvisioned(t, env, backendB)
}

// TestProvisionForeignVolume: the volume is bound to neither the helper nor the claim, it is never moved.
func TestProvisionForeignVolume(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.volumeHook = func(pv *corev1.PersistentVolume) {
		pv.Spec.ClaimRef.UID = foreignUID
	}

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	env.bindHelpers()

	_, state, err := env.provision("data", nodeA)
	if !errors.Is(err, errForeignVolume) || state != controller.ProvisioningFinished {
		t.Fatalf("Provision() = %v, %v; want %v with %v", state, err, controller.ProvisioningFinished, errForeignVolume)
	}

	pvs := env.listPVs()
	if len(pvs) != 1 || pvs[0].Spec.ClaimRef.UID != foreignUID {
		t.Errorf("PV claimRef changed: %+v", pvs)
	}
}

// TestProvisionSpread provisions several claims on different nodes, like a StatefulSet.
func TestProvisionSpread(t *testing.T) {
	env := newTestEnv(t, testCluster(
		testPVC("data-0"),
		testPVC("data-1"),
	)...)
	env.backend = true

	for name, node := range map[string]string{"data-0": nodeA, "data-1": nodeB} {
		pv, _, err := env.provisionUntilDone(name, node, 3)
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

// TestProvisionFaultPoints checks the fault injection points used by the e2e crash tests:
// each is reached once, right after its write, and not again when the step is skipped.
func TestProvisionFaultPoints(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.backend = true

	var points []string

	env.prov.SetFaultHook(func(point string) { points = append(points, point) })

	// A failed bind after the move: the next pass skips the move, so after-move is not reached again.
	env.failAt = 4

	if _, _, err := env.provisionUntilDone("data", nodeA, 5); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	if want := []string{FaultAfterMove, FaultAfterBind}; !slices.Equal(points, want) {
		t.Errorf("fault points = %v, want %v", points, want)
	}
}

// TestProvisionBoundByPVController: the PV controller binds the claim right after the move, before
// the provisioner does. The bind conflicts, sees that the claim is bound, and the same pass cleans up.
func TestProvisionBoundByPVController(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.backend = true
	env.bindOnMove = true

	pv, state, err := env.provisionUntilDone("data", nodeA, 3)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	if state != controller.ProvisioningFinished {
		t.Fatalf("Provision() state = %v, want %v", state, controller.ProvisioningFinished)
	}

	pvc, stored := requireProvisioned(t, env, backendA)

	if pv.Name != stored.Name {
		t.Errorf("returned PV = %s, want %s", pv.Name, stored.Name)
	}

	if _, err = env.getPVC(HelperName(pvc)); !apierrors.IsNotFound(err) {
		t.Errorf("helper PVC still exists, err = %v", err)
	}

	if slices.Contains(pvc.Finalizers, FinalizerProvisioning) {
		t.Errorf("user PVC still has finalizer %s", FinalizerProvisioning)
	}
}

// TestProvisionClaimGone: after the claim is deleted the library calls Provision with its stored copy.
// Provision stops for good and does not create a helper for the deleted claim.
func TestProvisionClaimGone(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("recreated %t", recreate), func(t *testing.T) {
			env := newTestEnv(t, testCluster(testPVC("data"))...)

			requirePending(t, env)

			opts := env.provisionOptions("data", nodeA)

			// The claim is deleted, its helper is deleted by the lifecycle controller.
			if err := env.client.CoreV1().PersistentVolumeClaims(testNamespace).Delete(t.Context(), "data", metav1.DeleteOptions{}); err != nil {
				t.Fatalf("failed to delete PVC: %v", err)
			}

			if err := env.client.CoreV1().PersistentVolumeClaims(testNamespace).Delete(t.Context(), HelperName(opts.PVC), metav1.DeleteOptions{}); err != nil {
				t.Fatalf("failed to delete helper PVC: %v", err)
			}

			if recreate {
				claim := testPVC("data")
				claim.UID = "" // a new UID is set on create

				if _, err := env.client.CoreV1().PersistentVolumeClaims(testNamespace).Create(t.Context(), claim, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to recreate PVC: %v", err)
				}
			}

			env.sync()

			_, state, err := env.prov.Provision(t.Context(), opts)
			if state != controller.ProvisioningFinished || !errors.As(err, new(*controller.IgnoredError)) {
				t.Fatalf("Provision() = %v, %v; want %v with an IgnoredError", state, err, controller.ProvisioningFinished)
			}

			if _, err = env.getPVC(HelperName(opts.PVC)); !apierrors.IsNotFound(err) {
				t.Errorf("helper PVC of the deleted claim is recreated, err = %v", err)
			}
		})
	}
}

// TestProvisionClaimDeleting: the claim is being deleted, but the library calls Provision with its
// stored copy without deletionTimestamp. The lifecycle controller owns the claim, it is not provisioned.
func TestProvisionClaimDeleting(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	requirePending(t, env)

	opts := env.provisionOptions("data", nodeA)

	claim := opts.PVC.DeepCopy()
	claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	if _, err := env.client.CoreV1().PersistentVolumeClaims(testNamespace).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update PVC: %v", err)
	}

	env.sync()

	writes := env.writes

	_, state, err := env.prov.Provision(t.Context(), opts)
	if state != controller.ProvisioningFinished || !errors.As(err, new(*controller.IgnoredError)) {
		t.Fatalf("Provision() = %v, %v; want %v with an IgnoredError", state, err, controller.ProvisioningFinished)
	}

	if env.writes != writes {
		t.Errorf("Provision() made %d writes to a claim that is being deleted, want 0", env.writes-writes)
	}
}

// requirePending runs one Provision() pass of the user PVC "data" on node-a and checks that it is still in progress.
func requirePending(t *testing.T, env *testEnv) {
	t.Helper()

	if _, state, err := env.provision("data", nodeA); err == nil || state != controller.ProvisioningInBackground {
		t.Fatalf("Provision() = %v, %v; want %v with error", state, err, controller.ProvisioningInBackground)
	}
}

// requireEvent checks that an event with the reason, and the text if not empty, is recorded.
func requireEvent(t *testing.T, events []string, reason, text string) {
	t.Helper()

	if !slices.ContainsFunc(events, func(ev string) bool {
		return strings.Contains(ev, " "+reason+" ") && strings.Contains(ev, text)
	}) {
		t.Errorf("events = %v, want %s %q", events, reason, text)
	}
}

// TestProvisionHelperTimeout: a pending helper is replaced and the claim is rescheduled after the helper timeout,
// at most testMaxReschedules times, then only events are emitted.
func TestProvisionHelperTimeout(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	for n := 1; n <= testMaxReschedules; n++ {
		env.selectNode()

		requirePending(t, env)

		// Not timed out yet.
		env.clock.SetTime(env.clock.Now().Add(testHelperTimeout - time.Minute))

		requirePending(t, env)

		env.clock.SetTime(env.clock.Now().Add(2 * time.Minute))

		_, state, err := env.provision("data", nodeA)
		if err == nil || state != controller.ProvisioningReschedule {
			t.Fatalf("Provision() = %v, %v after the timeout; want %v", state, err, controller.ProvisioningReschedule)
		}

		pvc, err := env.getPVC("data")
		if err != nil {
			t.Fatalf("failed to get user PVC: %v", err)
		}

		if got := pvc.Annotations[AnnotationReschedules]; got != strconv.Itoa(n) {
			t.Errorf("reschedules = %q, want %d", got, n)
		}

		for _, k := range []string{annotationSelectedNode, AnnotationBackendClass} {
			if _, ok := pvc.Annotations[k]; ok {
				t.Errorf("user PVC annotation %s is not removed", k)
			}
		}

		if _, err = env.getPVC(HelperName(pvc)); !apierrors.IsNotFound(err) {
			t.Errorf("helper PVC still exists, err = %v", err)
		}

		requireEvent(t, env.events(), ReasonRescheduled, fmt.Sprintf("(%d of %d)", n, testMaxReschedules))
	}

	// The limit is reached: the helper is kept and only events are emitted.
	env.selectNode()

	requirePending(t, env)

	env.clock.SetTime(env.clock.Now().Add(testHelperTimeout + time.Minute))

	requirePending(t, env)

	requireEvent(t, env.events(), ReasonHelperTimeout, "")

	if _, err := env.getPVC(HelperName(testPVC("data"))); err != nil {
		t.Errorf("helper PVC is deleted after the limit: %v", err)
	}

	// The backend provisions it eventually.
	env.backend = true

	if _, _, err := env.provisionUntilDone("data", nodeA, 3); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	requireProvisioned(t, env, backendA)
}

// TestProvisionBackendRejected: the backend removes the selected node from the helper when it cannot
// provision the volume there. The claim is rescheduled at once, at most testMaxReschedules times.
func TestProvisionBackendRejected(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	reject := func() {
		t.Helper()

		helper, err := env.getPVC(HelperName(testPVC("data")))
		if err != nil {
			t.Fatalf("failed to get helper PVC: %v", err)
		}

		delete(helper.Annotations, annotationSelectedNode)

		if _, err = env.client.CoreV1().PersistentVolumeClaims(testNamespace).Update(t.Context(), helper, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to update helper PVC: %v", err)
		}

		env.sync()
	}

	for n := 1; n <= testMaxReschedules; n++ {
		env.selectNode()

		requirePending(t, env)
		reject()

		_, state, err := env.provision("data", nodeA)
		if err == nil || state != controller.ProvisioningReschedule {
			t.Fatalf("Provision() = %v, %v after the backend rejected the node; want %v", state, err, controller.ProvisioningReschedule)
		}

		pvc, err := env.getPVC("data")
		if err != nil {
			t.Fatalf("failed to get user PVC: %v", err)
		}

		if got := pvc.Annotations[AnnotationReschedules]; got != strconv.Itoa(n) {
			t.Errorf("reschedules = %q, want %d", got, n)
		}

		if _, err = env.getPVC(HelperName(pvc)); !apierrors.IsNotFound(err) {
			t.Errorf("helper PVC still exists, err = %v", err)
		}

		requireEvent(t, env.events(), ReasonRescheduled, "rejected by the backend")
	}

	// The limit is reached: the helper is kept and only events are emitted.
	env.selectNode()

	requirePending(t, env)
	reject()
	requirePending(t, env)

	requireEvent(t, env.events(), ReasonBackendRejected, "")

	if _, err := env.getPVC(HelperName(testPVC("data"))); err != nil {
		t.Errorf("helper PVC is deleted after the limit: %v", err)
	}
}

// TestProvisionMirrorEvents: the Warning events of the helper are copied onto the claim once.
func TestProvisionMirrorEvents(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	helper, err := env.getPVC(HelperName(testPVC("data")))
	if err != nil {
		t.Fatalf("helper PVC not found: %v", err)
	}

	env.events()

	env.clock.SetTime(env.clock.Now().Add(time.Second))
	env.helperEvent(helper, corev1.EventTypeWarning, "ProvisioningFailed", "exceeded quota")
	env.helperEvent(helper, corev1.EventTypeNormal, "Provisioning", "external provisioner is provisioning")

	requirePending(t, env)

	events := env.events()
	requireEvent(t, events, ReasonWaitingForBackend, "ProvisioningFailed: exceeded quota")

	if len(events) != 1 {
		t.Errorf("events = %v, want only the Warning event mirrored", events)
	}

	// Already mirrored events are not repeated, new ones are.
	requirePending(t, env)

	if events = env.events(); len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}

	env.clock.SetTime(env.clock.Now().Add(time.Second))
	env.helperEvent(helper, corev1.EventTypeWarning, "ProvisioningFailed", "no space left")
	requirePending(t, env)

	events = env.events()
	requireEvent(t, events, ReasonWaitingForBackend, "no space left")

	if len(events) != 1 {
		t.Errorf("events = %v, want only the new event mirrored", events)
	}

	// A new event in the same second as the last mirrored one: timestamps have a resolution of a second.
	env.helperEvent(helper, corev1.EventTypeWarning, "ProvisioningFailed", "volume limit reached")
	requirePending(t, env)

	events = env.events()
	requireEvent(t, events, ReasonWaitingForBackend, "volume limit reached")

	if len(events) != 1 {
		t.Errorf("events = %v, want only the new event mirrored", events)
	}
}

// TestProvisionSelectedNodeChangedBound: the bound helper of the old node is kept if its volume fits the new node.
func TestProvisionSelectedNodeChangedBound(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.volumeHook = func(pv *corev1.PersistentVolume) {
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{nodeA, nodeB}
	}

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	env.bindHelpers()

	_, state, err := env.provision("data", nodeB)
	if err != nil || state != controller.ProvisioningFinished {
		t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningFinished)
	}

	// The volume of backend-a, provisioned for node-a, is used on node-b.
	requireProvisioned(t, env, backendA)
}

func TestProvisionMetrics(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.backend = true

	if _, _, err := env.provision("data", nodeA); err == nil {
		t.Fatalf("Provision() expected error")
	}

	env.bindHelpers()
	env.clock.SetTime(env.clock.Now().Add(time.Minute))

	if _, _, err := env.provisionUntilDone("data", nodeA, 3); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	for _, phase := range []string{phaseBackend, phaseHelper, phaseMove, phaseCleanup} {
		if got := testutil.ToFloat64(env.prov.metrics.phases.WithLabelValues(phase, resultSuccess)); got != 1 {
			t.Errorf("hybrid_provision_phase_total{phase=%q,result=success} = %v, want 1", phase, got)
		}
	}

	var m dto.Metric
	if err := env.prov.metrics.helperAge.Write(&m); err != nil {
		t.Fatalf("failed to read histogram: %v", err)
	}

	if m.GetHistogram().GetSampleCount() != 1 || m.GetHistogram().GetSampleSum() != time.Minute.Seconds() {
		t.Errorf("hybrid_helper_age_seconds count = %d sum = %v, want 1 and 60", m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum())
	}
}

// TestProvisionVolumeNodeAffinityLimit: a backend that ignores the selected node is not retried
// forever, after the reschedule limit the helper and its volume are kept and only events are emitted.
func TestProvisionVolumeNodeAffinityLimit(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)
	env.volumeHook = func(pv *corev1.PersistentVolume) {
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{nodeB}
	}

	for n := 1; n <= testMaxReschedules; n++ {
		env.selectNode()
		requirePending(t, env)
		env.bindHelpers()

		if _, state, err := env.provision("data", nodeA); err == nil || state != controller.ProvisioningReschedule {
			t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningReschedule)
		}
	}

	env.selectNode()
	requirePending(t, env)
	env.bindHelpers()
	env.events()

	requirePending(t, env)
	requireEvent(t, env.events(), ReasonVolumeNodeMismatch, "")

	pvc, err := env.getPVC("data")
	if err != nil {
		t.Fatalf("failed to get user PVC: %v", err)
	}

	if got := pvc.Annotations[AnnotationReschedules]; got != strconv.Itoa(testMaxReschedules) {
		t.Errorf("reschedules = %q, want %d", got, testMaxReschedules)
	}

	// The last volume is kept: no new volume is created and deleted on every pass.
	helper, err := env.getPVC(HelperName(pvc))
	if err != nil || helper.Spec.VolumeName == "" {
		t.Fatalf("helper PVC is not kept bound: %v, %v", helper, err)
	}

	if _, err = env.getPV(helper.Spec.VolumeName); err != nil {
		t.Errorf("volume of the helper is deleted: %v", err)
	}
}

// TestProvisionVolumeDeleted: the volume of a bound helper is deleted, a new helper is created.
func TestProvisionVolumeDeleted(t *testing.T) {
	env := newTestEnv(t, testCluster(testPVC("data"))...)

	requirePending(t, env)
	env.bindHelpers()

	helper, err := env.getPVC(HelperName(testPVC("data")))
	if err != nil {
		t.Fatalf("helper PVC not found: %v", err)
	}

	if err = env.client.CoreV1().PersistentVolumes().Delete(t.Context(), helper.Spec.VolumeName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete PV: %v", err)
	}

	requirePending(t, env)

	if _, err = env.getPVC(helper.Name); !apierrors.IsNotFound(err) {
		t.Fatalf("helper PVC with a deleted volume still exists, err = %v", err)
	}

	env.backend = true

	if _, _, err = env.provisionUntilDone("data", nodeA, 3); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	requireProvisioned(t, env, backendA)
}

// TestProvisionHelperTimeoutVolumeCreated: the backend has created the volume, but the PV controller
// has not bound the helper yet. The helper is deleted after the timeout, and the fresh volume of a Retain backend is deleted too.
func TestProvisionHelperTimeoutVolumeCreated(t *testing.T) {
	backend := testStorageClass(backendA, csiA)
	backend.ReclaimPolicy = new(corev1.PersistentVolumeReclaimRetain)

	env := newTestEnv(t,
		testNode(nodeA, nil),
		testCSIDriver(csiA),
		testCSINode(nodeA, csiA),
		backend,
		testHybridStorageClass(backendA),
		testPVC("data"),
	)

	requirePending(t, env)

	helper, err := env.getPVC(HelperName(testPVC("data")))
	if err != nil {
		t.Fatalf("helper PVC not found: %v", err)
	}

	env.inBackend = true
	pv := testBackendPV(helper, backend)

	if _, err = env.client.CoreV1().PersistentVolumes().Create(t.Context(), pv, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create PV: %v", err)
	}

	env.inBackend = false

	env.clock.SetTime(env.clock.Now().Add(testHelperTimeout + time.Minute))

	if _, state, err := env.provision("data", nodeA); err == nil || state != controller.ProvisioningReschedule {
		t.Fatalf("Provision() = %v, %v; want %v", state, err, controller.ProvisioningReschedule)
	}

	if policy := env.listPVs()[0].Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

// TestProvisionLegacyHelperDeleting: a v0.x helper that is being deleted is not adopted.
func TestProvisionLegacyHelperDeleting(t *testing.T) {
	claim := testPVC("data")

	helper := testLegacyHelper(claim, backendA)
	helper.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	helper.Finalizers = []string{finalizerPVCProtection}

	env := newTestEnv(t, testCluster(claim, helper)...)

	requirePending(t, env)

	if env.writes != 0 {
		t.Errorf("provisioner made %d writes, want 0", env.writes)
	}
}

// TestDeleteHelperGone: a helper deleted by someone else is forgotten.
func TestDeleteHelperGone(t *testing.T) {
	env := newTestEnv(t, testCluster()...)

	helper := testLegacyHelper(testPVC("data"), backendA)
	helper.CreationTimestamp = metav1.NewTime(env.clock.Now().Add(-time.Minute))
	env.prov.mirrored.Store(helper.UID, map[string]bool{})

	if err := env.prov.deleteHelper(t.Context(), helper, nil); err != nil {
		t.Fatalf("deleteHelper() error = %v", err)
	}

	if _, ok := env.prov.mirrored.Load(helper.UID); ok {
		t.Errorf("mirrored events of the deleted helper are not forgotten")
	}

	var m dto.Metric
	if err := env.prov.metrics.helperAge.Write(&m); err != nil {
		t.Fatalf("failed to read histogram: %v", err)
	}

	if m.GetHistogram().GetSampleCount() != 1 {
		t.Errorf("hybrid_helper_age_seconds count = %d, want 1", m.GetHistogram().GetSampleCount())
	}
}
