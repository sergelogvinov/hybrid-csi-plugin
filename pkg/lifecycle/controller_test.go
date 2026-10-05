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

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	recorder := record.NewFakeRecorder(100)
	prov := provisioner.NewProvisioner(ctx, client, provisioner.NewListers(factory), provisioner.Options{Recorder: recorder})

	ctrl, err := New(client, prov, factory, recorder, Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

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

// sync passes the claim to Reconcile: the claim deleted during provisioning is released.
func TestSync(t *testing.T) {
	claim := testClaim()
	claim.DeletionTimestamp = new(metav1.Now())
	helper := testHelper(claim)

	env := newTestEnv(t, append(testClasses(), claim, helper)...)

	env.sync(testClaimName)

	if env.getPVC(helper.Name) != nil {
		t.Errorf("helper PVC still exists")
	}

	env.requireEvent(provisioner.ReasonProvisioningCanceled)
}
