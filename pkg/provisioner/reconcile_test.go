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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

const testClaimName = "data"

// reconcileCluster has the hybrid StorageClass of one Retain backend, and the node selected for the claims.
func reconcileCluster(objs ...runtime.Object) []runtime.Object {
	backend := testStorageClass(backendA, csiA)
	backend.ReclaimPolicy = ptr.To(corev1.PersistentVolumeReclaimRetain)

	return append([]runtime.Object{
		testHybridStorageClass(backendA),
		backend,
		testNode(nodeA, map[string]string{corev1.LabelTopologyZone: "zone-a"}),
	}, objs...)
}

// provisioningClaim is a user claim that is being provisioned on node-a.
func provisioningClaim() *corev1.PersistentVolumeClaim {
	claim := testPVC(testClaimName)
	claim.Finalizers = []string{FinalizerProvisioning}
	claim.Annotations = map[string]string{
		AnnotationBackendClass: backendA,
		annotationSelectedNode: nodeA,
	}

	return claim
}

// pendingHelper is the helper of the claim, as Provision() creates it.
func pendingHelper(claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	helper := buildHelperPVC(claim, testStorageClass(backendA, csiA), nodeA)
	helper.UID = "uid-helper"
	helper.Finalizers = append(helper.Finalizers, finalizerPVCProtection)

	return helper
}

// boundHelper binds the helper to a fresh volume of the Retain backend.
func boundHelper(helper *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	backend := testStorageClass(backendA, csiA)
	backend.ReclaimPolicy = ptr.To(corev1.PersistentVolumeReclaimRetain)

	pv := testBackendPV(helper, backend)
	helper.Spec.VolumeName = pv.Name

	return helper, pv
}

// movedVolume is the volume after the move to the claim.
func movedVolume(pv *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Kind:       KindPersistentVolumeClaim,
		APIVersion: "v1",
		Namespace:  claim.Namespace,
		Name:       claim.Name,
		UID:        claim.UID,
	}

	return pv
}

// legacy turns the helper into a helper created by v0.x: no hybrid metadata, no owner reference.
func legacy(helper *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	helper.Labels = nil
	helper.Annotations = nil
	helper.Finalizers = []string{finalizerPVCProtection}
	helper.OwnerReferences = nil

	return helper
}

func deleting(pvc *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	pvc.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	return pvc
}

// newReconcileEnv is the fake cluster without the cleanup delay.
func newReconcileEnv(t *testing.T, objs ...runtime.Object) *testEnv {
	t.Helper()

	env := newTestEnv(t, reconcileCluster(objs...)...)
	env.prov.cleanupDelay = 0

	return env
}

// reconcile runs Reconcile for the PVC by name, as the lifecycle controller does.
func (e *testEnv) reconcile(name string) time.Duration {
	e.t.Helper()

	e.sync()

	pvc, err := e.listers.Claims.PersistentVolumeClaims(testNamespace).Get(name)
	if err != nil {
		e.t.Fatalf("failed to get PVC %s: %v", name, err)
	}

	after, err := e.prov.Reconcile(e.t.Context(), pvc)
	if err != nil {
		e.t.Fatalf("Reconcile(%s) error = %v", name, err)
	}

	return after
}

// pvc returns the PVC by name, or nil if it is gone.
func (e *testEnv) pvc(name string) *corev1.PersistentVolumeClaim {
	e.t.Helper()

	pvc, err := e.getPVC(name)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		e.t.Fatalf("failed to get PVC %s: %v", name, err)
	}

	return pvc
}

func (e *testEnv) pv(name string) *corev1.PersistentVolume {
	e.t.Helper()

	pv, err := e.getPV(name)
	if err != nil {
		e.t.Fatalf("failed to get PV %s: %v", name, err)
	}

	return pv
}

// requireReleased checks that the helper is gone and the claim has no provisioning finalizer.
func (e *testEnv) requireReleased(claim *corev1.PersistentVolumeClaim) {
	e.t.Helper()

	if helper := e.pvc(HelperName(claim)); helper != nil {
		e.t.Errorf("helper PVC still exists: %+v", helper.ObjectMeta)
	}

	if u := e.pvc(claim.Name); u != nil && slices.Contains(u.Finalizers, FinalizerProvisioning) {
		e.t.Errorf("user PVC still has finalizer %s", FinalizerProvisioning)
	}
}

// requireProvisioning checks that the claim stays in provisioning: the next Provision() pass creates a new helper.
func (e *testEnv) requireProvisioning() {
	e.t.Helper()

	if u := e.pvc(testClaimName); !slices.Contains(u.Finalizers, FinalizerProvisioning) || u.Spec.VolumeName != "" {
		e.t.Errorf("user PVC changed: %+v", u)
	}
}

// The claim is deleted before it is bound.

func TestReconcileClaimDeletedHelperPending(t *testing.T) {
	claim := deleting(provisioningClaim())
	env := newReconcileEnv(t, claim, pendingHelper(claim))

	env.reconcile(testClaimName)

	env.requireReleased(claim)
	requireEvent(t, env.events(), ReasonProvisioningCanceled, "")
}

func TestReconcileClaimDeletedHelperBound(t *testing.T) {
	claim := deleting(provisioningClaim())
	helper, pv := boundHelper(pendingHelper(claim))
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(testClaimName)

	env.requireReleased(claim)

	// The fresh volume of the Retain backend is deleted by the backend.
	if policy := env.pv(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

func TestReconcileClaimDeletedAfterMove(t *testing.T) {
	claim := deleting(provisioningClaim())
	helper, pv := boundHelper(pendingHelper(claim))
	pv = movedVolume(pv, claim) // The move is done: the volume is bound to the claim with the hybrid policy.
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(testClaimName)

	env.requireReleased(claim)

	// The volume belongs to the claim, it is reclaimed by the policy set by the move.
	if got := env.pv(pv.Name); got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || got.Spec.ClaimRef.UID != claim.UID {
		t.Errorf("PV changed: %+v", got.Spec)
	}
}

func TestReconcileClaimDeletedNoHelper(t *testing.T) {
	claim := deleting(provisioningClaim())
	env := newReconcileEnv(t, claim)

	env.reconcile(testClaimName)

	env.requireReleased(claim)
}

// The v0.x helper is not adopted yet: the claim was deleted after Provision() added the finalizer.
func TestReconcileClaimDeletedLegacyHelper(t *testing.T) {
	claim := deleting(provisioningClaim())
	helper, pv := boundHelper(legacy(pendingHelper(claim)))
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(testClaimName)

	env.requireReleased(claim)

	// The fresh volume of the Retain backend is deleted by the backend.
	if policy := env.pv(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

// A PVC with the helper name that does not request the volume of the claim is not a helper of v0.x.
func TestReconcileClaimDeletedForeignLegacyHelper(t *testing.T) {
	claim := deleting(provisioningClaim())
	foreign := legacy(pendingHelper(claim))
	foreign.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}
	env := newReconcileEnv(t, claim, foreign)

	env.reconcile(testClaimName)

	if env.pvc(foreign.Name) == nil {
		t.Errorf("foreign PVC is deleted")
	}
}

// The foreign PVC with the helper name is never touched.
func TestReconcileClaimDeletedForeignHelper(t *testing.T) {
	claim := deleting(provisioningClaim())
	foreign := pendingHelper(claim)
	foreign.Annotations[AnnotationOwnerUID] = foreignUID
	env := newReconcileEnv(t, claim, foreign)

	env.reconcile(testClaimName)

	if env.pvc(foreign.Name) == nil {
		t.Errorf("foreign PVC is deleted")
	}

	if u := env.pvc(testClaimName); u != nil && slices.Contains(u.Finalizers, FinalizerProvisioning) {
		t.Errorf("user PVC still has finalizer")
	}
}

// The helper is deleted by someone else.

func TestReconcileHelperDeletedPending(t *testing.T) {
	claim := provisioningClaim()
	helper := deleting(pendingHelper(claim))
	env := newReconcileEnv(t, claim, helper)

	env.reconcile(helper.Name)

	if env.pvc(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	env.requireProvisioning()
	requireEvent(t, env.events(), ReasonHelperDeleted, "")
}

func TestReconcileHelperDeletedBound(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(deleting(pendingHelper(claim)))
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(helper.Name)

	env.requireReleased(claim)

	// The volume is kept and moved to the claim.
	if u := env.pvc(testClaimName); u.Spec.VolumeName != pv.Name {
		t.Errorf("user PVC volumeName = %q, want %q", u.Spec.VolumeName, pv.Name)
	}

	got := env.pv(pv.Name)
	if got.Spec.ClaimRef.UID != claim.UID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete ||
		got.Labels[LabelManaged] != ValueTrue {
		t.Errorf("PV is not moved to the claim: %+v", got)
	}

	requireEvent(t, env.events(), ReasonVolumeRecovered, "")
}

// The volume of the deleted helper does not fit the selected node: it is discarded, not moved.
func TestReconcileHelperDeletedVolumeNodeMismatch(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(deleting(pendingHelper(claim)))
	pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-b"},
		}}}},
	}}

	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(helper.Name)

	if env.pvc(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	env.requireProvisioning()

	// The fresh volume is still bound to the helper, it is deleted by the backend.
	if got := env.pv(pv.Name); got.Spec.ClaimRef.UID != helper.UID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV is moved or kept: %+v", got.Spec)
	}

	requireEvent(t, env.events(), ReasonHelperDeleted, "")
}

// The volume is moved before the helper is deleted, and the claim has no selected node any more:
// the volume is the claim's own, it is not checked against a node.
func TestReconcileHelperDeletedAfterMove(t *testing.T) {
	claim := provisioningClaim()
	delete(claim.Annotations, annotationSelectedNode)
	helper, pv := boundHelper(deleting(pendingHelper(claim)))
	pv = movedVolume(pv, claim)
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(helper.Name)

	env.requireReleased(claim)

	if u := env.pvc(testClaimName); u.Spec.VolumeName != pv.Name {
		t.Errorf("user PVC volumeName = %q, want %q", u.Spec.VolumeName, pv.Name)
	}

	requireEvent(t, env.events(), ReasonVolumeRecovered, "")
}

func TestReconcileHelperDeletedOwnerGone(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(deleting(pendingHelper(claim)))
	env := newReconcileEnv(t, helper, pv)

	env.reconcile(helper.Name)

	if env.pvc(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if policy := env.pv(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

// A new claim with the same name is not the owner of the helper.
func TestReconcileHelperDeletedOwnerReplaced(t *testing.T) {
	claim := provisioningClaim()
	helper := deleting(pendingHelper(claim))

	replaced := provisioningClaim()
	replaced.UID = types.UID("uid-new")

	env := newReconcileEnv(t, replaced, helper)

	env.reconcile(helper.Name)

	if env.pvc(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if events := env.events(); len(events) != 0 {
		t.Errorf("events = %v, want none on the new claim", events)
	}
}

func TestReconcileHelperDeletedForeignVolume(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(deleting(pendingHelper(claim)))
	pv.Spec.ClaimRef.UID = foreignUID
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(helper.Name)

	if env.pvc(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if got := env.pv(pv.Name); got.Spec.ClaimRef.UID != foreignUID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("foreign PV changed: %+v", got.Spec)
	}

	if u := env.pvc(testClaimName); u.Spec.VolumeName != "" {
		t.Errorf("user PVC is bound to %q", u.Spec.VolumeName)
	}
}

// Leftovers after the claim is bound.

func TestReconcileBoundClaimLeftovers(t *testing.T) {
	tests := []struct {
		name   string
		helper bool
		key    func(claim *corev1.PersistentVolumeClaim) string
	}{
		{name: "helper left, reconcile claim", helper: true, key: func(c *corev1.PersistentVolumeClaim) string { return c.Name }},
		{name: "helper left, reconcile helper", helper: true, key: HelperName},
		{name: "finalizer left", key: func(c *corev1.PersistentVolumeClaim) string { return c.Name }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := provisioningClaim()
			helper, pv := boundHelper(pendingHelper(claim))
			pv = movedVolume(pv, claim)
			claim.Spec.VolumeName = pv.Name

			objs := []runtime.Object{claim, pv}
			if tt.helper {
				objs = append(objs, helper)
			}

			env := newReconcileEnv(t, objs...)

			env.reconcile(tt.key(claim))

			env.requireReleased(claim)
			requireEvent(t, env.events(), ReasonCleanupFinished, "")
		})
	}
}

// The Provision() pass that has just bound the claim finishes the cleanup itself, the cleanup waits.
func TestReconcileBoundClaimCleanupDelay(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(pendingHelper(claim))
	pv = movedVolume(pv, claim)
	claim.Spec.VolumeName = pv.Name

	env := newReconcileEnv(t, claim, pv, helper)
	env.prov.cleanupDelay = time.Minute

	if after := env.reconcile(testClaimName); after != time.Minute {
		t.Errorf("Reconcile() = %v, want to wait %v", after, time.Minute)
	}

	if env.writes != 0 {
		t.Errorf("Reconcile() made %d writes, want 0", env.writes)
	}

	env.clock.SetTime(env.clock.Now().Add(time.Minute))

	if after := env.reconcile(testClaimName); after != 0 {
		t.Errorf("Reconcile() = %v after the delay, want 0", after)
	}

	env.requireReleased(claim)
}

// A claim that is being provisioned belongs to Provision().
func TestReconcileClaimProvisioning(t *testing.T) {
	claim := provisioningClaim()
	helper, pv := boundHelper(pendingHelper(claim))
	env := newReconcileEnv(t, claim, helper, pv)

	env.reconcile(testClaimName)
	env.reconcile(helper.Name)

	if env.writes != 0 {
		t.Errorf("Reconcile() made %d writes, want 0", env.writes)
	}
}
