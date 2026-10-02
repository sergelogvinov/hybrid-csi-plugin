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

package lifecycle

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

const (
	testNamespace   = "default"
	testHybridClass = "hybrid"
	testBackend     = "backend-a"
	testDriver      = "csi.a.example.com"
	testClaimName   = "data"
	testNodeName    = "node-a"
	testZoneLabel   = "topology.kubernetes.io/zone"
	foreignUID      = "someone-else"
)

// testEnv is a fake cluster with the lifecycle controller under test.
type testEnv struct {
	t        *testing.T
	client   *fake.Clientset
	ctrl     *Controller
	recorder *record.FakeRecorder
	writes   int
}

func newTestEnv(t *testing.T, objs ...runtime.Object) *testEnv {
	t.Helper()

	ctx := t.Context()

	client := fake.NewClientset(objs...)
	factory := informers.NewSharedInformerFactory(client, 0)
	prov := provisioner.NewProvisioner(ctx, client, provisioner.NewListers(factory), provisioner.Options{})
	recorder := record.NewFakeRecorder(100)

	ctrl, err := New(client, prov, factory, recorder, Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctrl.cleanupDelay = 0

	t.Cleanup(ctrl.queue.ShutDown)

	factory.Start(ctx.Done())

	for typ, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			t.Fatalf("failed to sync informer %v", typ)
		}
	}

	e := &testEnv{t: t, client: client, ctrl: ctrl, recorder: recorder}

	client.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		switch action.GetVerb() {
		case "create", "update", "patch", "delete":
			e.writes++
		}

		return false, nil, nil
	})

	return e
}

// sync reconciles the PVC by name, as a worker does.
func (e *testEnv) sync(name string) {
	e.t.Helper()

	if err := e.ctrl.sync(e.t.Context(), testNamespace+"/"+name); err != nil {
		e.t.Fatalf("sync(%s) error = %v", name, err)
	}
}

func (e *testEnv) getPVC(name string) *corev1.PersistentVolumeClaim {
	e.t.Helper()

	pvc, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(e.t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		e.t.Fatalf("failed to get PVC %s: %v", name, err)
	}

	return pvc
}

func (e *testEnv) getPV(name string) *corev1.PersistentVolume {
	e.t.Helper()

	pv, err := e.client.CoreV1().PersistentVolumes().Get(e.t.Context(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("failed to get PV %s: %v", name, err)
	}

	return pv
}

// events returns the events recorded so far.
func (e *testEnv) events() []string {
	var events []string

	for {
		select {
		case ev := <-e.recorder.Events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

func (e *testEnv) requireEvent(reason string) {
	e.t.Helper()

	events := e.events()
	if !slices.ContainsFunc(events, func(ev string) bool { return strings.Contains(ev, " "+reason+" ") }) {
		e.t.Errorf("events = %v, want %s", events, reason)
	}
}

// Fixtures.

func testStorageClass(name, provisionerName string, policy corev1.PersistentVolumeReclaimPolicy) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:    metav1.ObjectMeta{Name: name},
		Provisioner:   provisionerName,
		ReclaimPolicy: &policy,
	}
}

// testClasses returns the hybrid and backend StorageClasses, and the node selected for the claims.
func testClasses() []runtime.Object {
	hsc := testStorageClass(testHybridClass, provisioner.DriverName, corev1.PersistentVolumeReclaimDelete)
	hsc.Parameters = map[string]string{"storageClasses": testBackend}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Labels: map[string]string{testZoneLabel: "zone-a"}},
	}

	return []runtime.Object{hsc, testStorageClass(testBackend, testDriver, corev1.PersistentVolumeReclaimRetain), node}
}

// testClaim is a user claim that is being provisioned.
func testClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testClaimName,
			Namespace:  testNamespace,
			UID:        "uid-data",
			Finalizers: []string{provisioner.FinalizerProvisioning},
			Annotations: map[string]string{
				provisioner.AnnotationBackendClass:   testBackend,
				"volume.kubernetes.io/selected-node": testNodeName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: new(testHybridClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

// testHelper is the pending helper of the claim.
func testHelper(claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        provisioner.HelperName(claim),
			Namespace:   claim.Namespace,
			UID:         "uid-helper",
			Labels:      map[string]string{provisioner.LabelRole: provisioner.LabelRoleHelper},
			Annotations: map[string]string{provisioner.AnnotationOwnerUID: string(claim.UID)},
			Finalizers:  []string{provisioner.FinalizerHelper, "kubernetes.io/pvc-protection"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: new(true),
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      claim.Spec.AccessModes,
			StorageClassName: new(testBackend),
			Resources:        claim.Spec.Resources,
		},
	}
}

// testVolume is the backend volume bound to the claim.
func testVolume(claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pv-backend",
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": testDriver},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:                   claim.Spec.AccessModes,
			Capacity:                      claim.Spec.Resources.Requests,
			StorageClassName:              testBackend,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Kind:       "PersistentVolumeClaim",
				APIVersion: "v1",
				Namespace:  claim.Namespace,
				Name:       claim.Name,
				UID:        claim.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: "vol"},
			},
		},
	}
}

// legacy turns the helper into a helper created by v0.x: no hybrid metadata, no owner reference.
func legacy(helper *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	helper.Labels = nil
	helper.Annotations = nil
	helper.Finalizers = []string{"kubernetes.io/pvc-protection"}
	helper.OwnerReferences = nil

	return helper
}

// bound binds the helper to the volume.
func bound(helper *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	pv := testVolume(helper)
	helper.Spec.VolumeName = pv.Name

	return helper, pv
}

func deleting(pvc *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	pvc.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	return pvc
}

// requireReleased checks that the helper is gone and the claim has no provisioning finalizer.
func (e *testEnv) requireReleased(claim *corev1.PersistentVolumeClaim) {
	e.t.Helper()

	if helper := e.getPVC(provisioner.HelperName(claim)); helper != nil {
		e.t.Errorf("helper PVC still exists: %+v", helper.ObjectMeta)
	}

	if u := e.getPVC(claim.Name); u != nil && slices.Contains(u.Finalizers, provisioner.FinalizerProvisioning) {
		e.t.Errorf("user PVC still has finalizer %s", provisioner.FinalizerProvisioning)
	}
}

// The claim is deleted before it is bound.

func TestClaimDeletedHelperPending(t *testing.T) {
	claim := deleting(testClaim())
	env := newTestEnv(t, append(testClasses(), claim, testHelper(claim))...)

	env.sync(testClaimName)

	env.requireReleased(claim)
	env.requireEvent(ReasonProvisioningCanceled)
}

func TestClaimDeletedHelperBound(t *testing.T) {
	claim := deleting(testClaim())
	helper, pv := bound(testHelper(claim))
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(testClaimName)

	env.requireReleased(claim)

	// The fresh volume of the Retain backend is deleted by the backend.
	if policy := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

func TestClaimDeletedAfterMove(t *testing.T) {
	claim := deleting(testClaim())
	helper, _ := bound(testHelper(claim))
	pv := testVolume(claim) // The move is done: the volume is bound to the claim with the hybrid policy.
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(testClaimName)

	env.requireReleased(claim)

	// The volume belongs to the claim, it is reclaimed by the policy set by the move.
	if got := env.getPV(pv.Name); got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || got.Spec.ClaimRef.UID != claim.UID {
		t.Errorf("PV changed: %+v", got.Spec)
	}
}

func TestClaimDeletedNoHelper(t *testing.T) {
	claim := deleting(testClaim())
	env := newTestEnv(t, append(testClasses(), claim)...)

	env.sync(testClaimName)

	env.requireReleased(claim)
}

// The v0.x helper is not adopted yet: the claim was deleted after Provision() added the finalizer.
func TestClaimDeletedLegacyHelper(t *testing.T) {
	claim := deleting(testClaim())
	helper, pv := bound(legacy(testHelper(claim)))
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(testClaimName)

	env.requireReleased(claim)

	// The fresh volume of the Retain backend is deleted by the backend.
	if policy := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

// A PVC with the helper name that does not request the volume of the claim is not a helper of v0.x.
func TestClaimDeletedForeignLegacyHelper(t *testing.T) {
	claim := deleting(testClaim())
	foreign := legacy(testHelper(claim))
	foreign.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}
	env := newTestEnv(t, append(testClasses(), claim, foreign)...)

	env.sync(testClaimName)

	if env.getPVC(foreign.Name) == nil {
		t.Errorf("foreign PVC is deleted")
	}
}

// The foreign PVC with the helper name is never touched.
func TestClaimDeletedForeignHelper(t *testing.T) {
	claim := deleting(testClaim())
	foreign := testHelper(claim)
	foreign.Annotations[provisioner.AnnotationOwnerUID] = foreignUID
	env := newTestEnv(t, append(testClasses(), claim, foreign)...)

	env.sync(testClaimName)

	if env.getPVC(foreign.Name) == nil {
		t.Errorf("foreign PVC is deleted")
	}

	if u := env.getPVC(testClaimName); u != nil && slices.Contains(u.Finalizers, provisioner.FinalizerProvisioning) {
		t.Errorf("user PVC still has finalizer")
	}
}

// The helper is deleted by someone else.

func TestHelperDeletedPending(t *testing.T) {
	claim := testClaim()
	helper := deleting(testHelper(claim))
	env := newTestEnv(t, append(testClasses(), claim, helper)...)

	env.sync(helper.Name)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	// The claim stays in provisioning, the next Provision() pass creates a new helper.
	if u := env.getPVC(testClaimName); !slices.Contains(u.Finalizers, provisioner.FinalizerProvisioning) || u.Spec.VolumeName != "" {
		t.Errorf("user PVC changed: %+v", u)
	}

	env.requireEvent(ReasonHelperDeleted)
}

func TestHelperDeletedBound(t *testing.T) {
	claim := testClaim()
	helper, pv := bound(deleting(testHelper(claim)))
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(helper.Name)

	env.requireReleased(claim)

	// The volume is kept and moved to the claim.
	u := env.getPVC(testClaimName)
	if u.Spec.VolumeName != pv.Name {
		t.Errorf("user PVC volumeName = %q, want %q", u.Spec.VolumeName, pv.Name)
	}

	got := env.getPV(pv.Name)
	if got.Spec.ClaimRef.UID != claim.UID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete ||
		got.Labels[provisioner.LabelManaged] != provisioner.ValueTrue {
		t.Errorf("PV is not moved to the claim: %+v", got)
	}

	env.requireEvent(ReasonVolumeRecovered)
}

// The volume of the deleted helper does not fit the selected node: it is discarded, not moved.
func TestHelperDeletedVolumeNodeMismatch(t *testing.T) {
	claim := testClaim()
	helper, pv := bound(deleting(testHelper(claim)))
	pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: testZoneLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"zone-b"},
		}}}},
	}}

	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(helper.Name)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	// The claim stays in provisioning, the next Provision() pass creates a new helper.
	if u := env.getPVC(testClaimName); !slices.Contains(u.Finalizers, provisioner.FinalizerProvisioning) || u.Spec.VolumeName != "" {
		t.Errorf("user PVC changed: %+v", u)
	}

	// The fresh volume is still bound to the helper, it is deleted by the backend.
	got := env.getPV(pv.Name)
	if got.Spec.ClaimRef.UID != helper.UID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV is moved or kept: %+v", got.Spec)
	}

	env.requireEvent(ReasonHelperDeleted)
}

func TestHelperDeletedOwnerGone(t *testing.T) {
	claim := testClaim()
	helper, pv := bound(deleting(testHelper(claim)))
	env := newTestEnv(t, append(testClasses(), helper, pv)...)

	env.sync(helper.Name)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if policy := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; policy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV reclaim policy = %s, want Delete", policy)
	}
}

// A new claim with the same name is not the owner of the helper.
func TestHelperDeletedOwnerReplaced(t *testing.T) {
	claim := testClaim()
	helper := deleting(testHelper(claim))

	replaced := testClaim()
	replaced.UID = types.UID("uid-new")

	env := newTestEnv(t, append(testClasses(), replaced, helper)...)

	env.sync(helper.Name)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if events := env.events(); len(events) != 0 {
		t.Errorf("events = %v, want none on the new claim", events)
	}
}

func TestHelperDeletedForeignVolume(t *testing.T) {
	claim := testClaim()
	helper, pv := bound(deleting(testHelper(claim)))
	pv.Spec.ClaimRef.UID = foreignUID
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(helper.Name)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	if got := env.getPV(pv.Name); got.Spec.ClaimRef.UID != foreignUID || got.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("foreign PV changed: %+v", got.Spec)
	}

	if u := env.getPVC(testClaimName); u.Spec.VolumeName != "" {
		t.Errorf("user PVC is bound to %q", u.Spec.VolumeName)
	}
}

// Leftovers after the claim is bound.

func TestBoundClaimLeftovers(t *testing.T) {
	tests := []struct {
		name   string
		helper bool
		key    func(claim *corev1.PersistentVolumeClaim) string
	}{
		{name: "helper left, sync claim", helper: true, key: func(c *corev1.PersistentVolumeClaim) string { return c.Name }},
		{name: "helper left, sync helper", helper: true, key: provisioner.HelperName},
		{name: "finalizer left", key: func(c *corev1.PersistentVolumeClaim) string { return c.Name }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := testClaim()
			pv := testVolume(claim)
			claim.Spec.VolumeName = pv.Name

			objs := append(testClasses(), claim, pv)

			if tt.helper {
				helper, _ := bound(testHelper(claim))
				objs = append(objs, helper)
			}

			env := newTestEnv(t, objs...)

			env.sync(tt.key(claim))

			env.requireReleased(claim)
			env.requireEvent(ReasonCleanupFinished)
		})
	}
}

// The Provision() pass that has just bound the claim finishes the cleanup itself, the cleanup waits.
func TestBoundClaimCleanupDelay(t *testing.T) {
	claim := testClaim()
	pv := testVolume(claim)
	claim.Spec.VolumeName = pv.Name
	helper, _ := bound(testHelper(claim))

	env := newTestEnv(t, append(testClasses(), claim, pv, helper)...)
	env.ctrl.cleanupDelay = time.Hour

	env.sync(testClaimName)

	if env.writes != 0 {
		t.Errorf("controller made %d writes, want 0", env.writes)
	}

	env.ctrl.cleanupDelay = 0
	env.sync(testClaimName)

	env.requireReleased(claim)
}

// A claim that is being provisioned belongs to Provision().
func TestClaimProvisioning(t *testing.T) {
	claim := testClaim()
	helper, pv := bound(testHelper(claim))
	env := newTestEnv(t, append(testClasses(), claim, helper, pv)...)

	env.sync(testClaimName)
	env.sync(helper.Name)

	if env.writes != 0 {
		t.Errorf("controller made %d writes, want 0", env.writes)
	}
}

func TestEnqueue(t *testing.T) {
	claim := testClaim()
	helper := testHelper(claim)

	plain := testClaim()
	plain.Name = "plain"
	plain.Finalizers = nil

	env := newTestEnv(t, testClasses()...)

	env.ctrl.enqueueClaim(plain)

	if n := env.ctrl.queue.Len(); n != 0 {
		t.Errorf("queue length = %d after a claim without finalizer, want 0", n)
	}

	env.ctrl.enqueueClaim(helper)

	if n := env.ctrl.queue.Len(); n != 2 {
		t.Errorf("queue length = %d after a helper, want 2 (helper and its owner)", n)
	}
}
