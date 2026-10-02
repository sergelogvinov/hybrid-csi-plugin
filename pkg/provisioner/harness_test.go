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
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-helpers/storage/volume"
	testingclock "k8s.io/utils/clock/testing"
)

const (
	testNamespace   = "default"
	testHybridClass = "hybrid"

	testHelperTimeout  = 10 * time.Minute
	testMaxReschedules = 2
)

// testEnv is a fake cluster: fake clientset, started informers and the provisioner under test.
type testEnv struct {
	t       *testing.T
	client  *fake.Clientset
	listers Listers
	prov    *HybridProvisioner

	recorder *record.FakeRecorder
	clock    *testingclock.FakePassiveClock

	// backend binds the pending helper PVCs between Provision() passes.
	backend bool
	// bindOnMove makes the emulated PV controller bind the claim as soon as the volume is moved
	// to it, before the provisioner does it.
	bindOnMove bool
	// volumeHook changes the backend PV before it is created.
	volumeHook func(pv *corev1.PersistentVolume)
	// inBackend is set while the simulated backend writes, its writes are not counted.
	inBackend bool
	// writes is the number of API writes made by the provisioner.
	writes int
	// failAt fails the n-th write of the provisioner, 0 disables it.
	failAt int
	// version is the last resourceVersion set by the emulated API server.
	version int
}

// newTestEnv creates the fake cluster with the objects and the provisioner.
func newTestEnv(t *testing.T, objs ...runtime.Object) *testEnv {
	t.Helper()

	ctx := t.Context()

	client := fake.NewClientset(objs...)
	factory := informers.NewSharedInformerFactory(client, 0)
	listers := NewListers(factory)

	factory.Start(ctx.Done())

	for typ, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			t.Fatalf("failed to sync informer %v", typ)
		}
	}

	recorder := record.NewFakeRecorder(100)
	prov := NewProvisioner(ctx, client, listers, Options{
		HelperTimeout:        testHelperTimeout,
		HelperMaxReschedules: testMaxReschedules,
		Recorder:             recorder,
	})

	clock := testingclock.NewFakePassiveClock(time.Now())
	prov.clock = clock

	e := &testEnv{t: t, client: client, listers: listers, prov: prov, recorder: recorder, clock: clock}

	// Reactors run in reverse order: countWrites (fault injection), then apiServer.
	client.PrependReactor("*", "*", e.apiServer)
	client.PrependReactor("*", "*", e.countWrites)

	return e
}

// countWrites counts the writes of the provisioner and fails the one at failAt.
func (e *testEnv) countWrites(action clienttesting.Action) (bool, runtime.Object, error) {
	switch action.GetVerb() {
	case "create", "update", "patch", "delete":
	default:
		return false, nil, nil
	}

	if e.inBackend {
		return false, nil, nil
	}

	e.writes++

	if e.writes == e.failAt {
		return true, nil, fmt.Errorf("injected fault: %s %s", action.GetVerb(), action.GetResource().Resource)
	}

	return false, nil, nil
}

// apiServer emulates what the fake clientset does not: the metadata the API server sets
// on create (UID, creation time), and optimistic concurrency: every create and update stores
// a copy with a new resourceVersion, an update with another resourceVersion is a conflict.
// The caller's object is never changed, as with a real client.
//
// It also emulates the writes of the PV controller right after the move: the helper becomes
// Lost when the volume is moved to the claim, the claim becomes Bound when it points at the volume,
// so the objects read before are stale.
func (e *testEnv) apiServer(action clienttesting.Action) (bool, runtime.Object, error) {
	tracker := e.client.Tracker()
	gvr := action.GetResource()

	// CreateAction and UpdateAction have the same methods, a type switch cannot tell them apart.
	a, ok := action.(clienttesting.CreateAction)
	if !ok {
		return false, nil, nil
	}

	switch action.GetVerb() {
	case "create":
		obj := a.GetObject().DeepCopyObject()

		meta, ok := obj.(metav1.Object)
		if !ok {
			return false, nil, nil
		}

		version := e.nextVersion()

		// Unique, as a recreated object gets a new UID.
		if meta.GetUID() == "" {
			meta.SetUID(types.UID("uid-" + meta.GetName() + "-" + version))
		}

		if ts := meta.GetCreationTimestamp(); ts.IsZero() {
			meta.SetCreationTimestamp(metav1.NewTime(e.clock.Now()))
		}

		meta.SetResourceVersion(version)

		if err := tracker.Create(gvr, obj, a.GetNamespace()); err != nil {
			return true, nil, err
		}

		return true, obj.DeepCopyObject(), nil

	case "update":
		obj := a.GetObject().DeepCopyObject()

		meta, ok := obj.(metav1.Object)
		if !ok {
			return false, nil, nil
		}

		stored, err := tracker.Get(gvr, a.GetNamespace(), meta.GetName())
		if err != nil {
			return true, nil, err
		}

		if storedMeta, ok := stored.(metav1.Object); ok && meta.GetResourceVersion() != "" && meta.GetResourceVersion() != storedMeta.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(gvr.GroupResource(), meta.GetName(),
				fmt.Errorf("resourceVersion %s is stale, stored %s", meta.GetResourceVersion(), storedMeta.GetResourceVersion()))
		}

		meta.SetResourceVersion(e.nextVersion())

		if err = tracker.Update(gvr, obj, a.GetNamespace()); err != nil {
			return true, nil, err
		}

		if a.GetSubresource() == "" {
			e.pvController(obj)
		}

		return true, obj.DeepCopyObject(), nil
	}

	return false, nil, nil
}

func (e *testEnv) nextVersion() string {
	e.version++

	return strconv.Itoa(e.version)
}

// pvController emulates the PV controller after an update of the object, see apiServer.
func (e *testEnv) pvController(obj runtime.Object) {
	tracker := e.client.Tracker()
	pvcs := corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")

	var claim *corev1.PersistentVolumeClaim

	switch obj := obj.(type) {
	case *corev1.PersistentVolume:
		if obj.Spec.ClaimRef == nil {
			return
		}

		stored, err := tracker.Get(pvcs, obj.Spec.ClaimRef.Namespace, "pvc-"+string(obj.Spec.ClaimRef.UID))
		helper, ok := stored.(*corev1.PersistentVolumeClaim)

		if err != nil || !ok || helper.Spec.VolumeName != obj.Name || helper.Status.Phase == corev1.ClaimLost {
			return
		}

		claim = helper.DeepCopy()
		claim.Status.Phase = corev1.ClaimLost

		if e.bindOnMove {
			e.bindMovedClaim(obj)
		}
	case *corev1.PersistentVolumeClaim:
		if obj.Spec.VolumeName == "" || obj.Labels[LabelRole] == LabelRoleHelper || obj.Status.Phase == corev1.ClaimBound {
			return
		}

		claim = obj.DeepCopy()
		claim.Status.Phase = corev1.ClaimBound
	default:
		return
	}

	claim.ResourceVersion = e.nextVersion()

	if err := tracker.Update(pvcs, claim, claim.Namespace); err != nil {
		e.t.Errorf("pv controller: failed to update %s: %v", claim.Name, err)
	}
}

// bindMovedClaim emulates the PV controller binding the claim to the volume pre-bound to it.
func (e *testEnv) bindMovedClaim(pv *corev1.PersistentVolume) {
	tracker := e.client.Tracker()
	pvcs := corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")

	stored, err := tracker.Get(pvcs, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	claim, ok := stored.(*corev1.PersistentVolumeClaim)

	if err != nil || !ok || claim.UID != pv.Spec.ClaimRef.UID || claim.Spec.VolumeName != "" {
		return
	}

	claim = claim.DeepCopy()
	claim.Spec.VolumeName = pv.Name
	claim.Status.Phase = corev1.ClaimBound

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}

	claim.Annotations[volume.AnnBindCompleted] = valueYes
	claim.Annotations[volume.AnnBoundByController] = valueYes
	claim.ResourceVersion = e.nextVersion()

	if err := tracker.Update(pvcs, claim, claim.Namespace); err != nil {
		e.t.Errorf("pv controller: failed to bind %s: %v", claim.Name, err)
	}
}

// events returns the events recorded on the claims so far.
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

// bindHelpers emulates the backend provisioner and the PV controller:
// a PV is created for every pending helper PVC and bound to it.
func (e *testEnv) bindHelpers() {
	e.t.Helper()

	e.inBackend = true
	defer func() { e.inBackend = false }()

	helpers, err := e.client.CoreV1().PersistentVolumeClaims("").List(e.t.Context(), metav1.ListOptions{
		LabelSelector: LabelRole + "=" + LabelRoleHelper,
	})
	if err != nil {
		e.t.Fatalf("backend: failed to list helper PVCs: %v", err)
	}

	for i := range helpers.Items {
		if helper := &helpers.Items[i]; helper.Spec.VolumeName == "" && helper.DeletionTimestamp == nil {
			e.bindHelper(helper)
		}
	}
}

// bindHelper creates a backend PV for the helper PVC and binds them.
func (e *testEnv) bindHelper(helper *corev1.PersistentVolumeClaim) {
	e.t.Helper()

	pvcs := e.client.CoreV1().PersistentVolumeClaims(helper.Namespace)

	sc, err := e.client.StorageV1().StorageClasses().Get(e.t.Context(), *helper.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("backend: failed to get storage class %s: %v", *helper.Spec.StorageClassName, err)
	}

	pv := testBackendPV(helper, sc)
	if e.volumeHook != nil {
		e.volumeHook(pv)
	}

	if _, err = e.client.CoreV1().PersistentVolumes().Create(e.t.Context(), pv, metav1.CreateOptions{}); err != nil {
		e.t.Fatalf("backend: failed to create PV %s: %v", pv.Name, err)
	}

	helper = helper.DeepCopy()
	helper.Spec.VolumeName = pv.Name
	helper.Status.Phase = corev1.ClaimBound
	helper.Finalizers = append(helper.Finalizers, finalizerPVCProtection)

	if helper, err = pvcs.Update(e.t.Context(), helper, metav1.UpdateOptions{}); err != nil {
		e.t.Fatalf("backend: failed to update helper PVC: %v", err)
	}

	if helper.Status.Phase != corev1.ClaimBound {
		helper.Status.Phase = corev1.ClaimBound

		if _, err = pvcs.UpdateStatus(e.t.Context(), helper, metav1.UpdateOptions{}); err != nil {
			e.t.Fatalf("backend: failed to update helper PVC status: %v", err)
		}
	}
}

// sync waits until the informer caches have caught up with the fake cluster.
func (e *testEnv) sync() {
	e.t.Helper()

	err := wait.PollUntilContextTimeout(e.t.Context(), 5*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		claims, err := e.client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		cachedClaims, err := e.listers.Claims.List(labels.Everything())
		if err != nil {
			return false, err
		}

		volumes, err := e.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		cachedVolumes, err := e.listers.Volumes.List(labels.Everything())
		if err != nil {
			return false, err
		}

		return sameObjects(claims.Items, cachedClaims) && sameObjects(volumes.Items, cachedVolumes), nil
	})
	if err != nil {
		e.t.Fatalf("informers did not sync: %v", err)
	}
}

func sameObjects[T any, PT interface {
	*T
	metav1.Object
}](items []T, cached []PT) bool {
	if len(items) != len(cached) {
		return false
	}

	byName := make(map[string]PT, len(cached))
	for _, obj := range cached {
		byName[obj.GetNamespace()+"/"+obj.GetName()] = obj
	}

	for i := range items {
		obj := PT(&items[i])
		if c, ok := byName[obj.GetNamespace()+"/"+obj.GetName()]; !ok || !apiequality.Semantic.DeepEqual(obj, c) {
			return false
		}
	}

	return true
}

// provision runs one Provision() pass for the user PVC, as the library does.
func (e *testEnv) provision(pvcName, nodeName string) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	e.t.Helper()

	e.sync()

	return e.prov.Provision(e.t.Context(), e.provisionOptions(pvcName, nodeName))
}

// provisionUntilDone calls Provision() until it stops returning ProvisioningInBackground
// or the user PVC is bound (the library does not call Provision() for it any more).
func (e *testEnv) provisionUntilDone(pvcName, nodeName string, maxPasses int) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	e.t.Helper()

	var (
		pv    *corev1.PersistentVolume
		state controller.ProvisioningState
		err   error
	)

	for range maxPasses {
		pv, state, err = e.provision(pvcName, nodeName)
		if state != controller.ProvisioningInBackground {
			return pv, state, err
		}

		claim, getErr := e.getPVC(pvcName)
		if getErr != nil {
			e.t.Fatalf("failed to get PVC %s: %v", pvcName, getErr)
		}

		if claim.Spec.VolumeName != "" {
			return pv, state, err
		}

		if e.backend {
			e.bindHelpers()
		}
	}

	e.t.Fatalf("Provision() did not converge in %d passes, last error: %v", maxPasses, err)

	return nil, state, err
}

// provisionOptions builds the options the library passes to Provision() for the user PVC.
func (e *testEnv) provisionOptions(pvcName, nodeName string) controller.ProvisionOptions {
	e.t.Helper()

	pvc, err := e.getPVC(pvcName)
	if err != nil {
		e.t.Fatalf("failed to get PVC %s: %v", pvcName, err)
	}

	node, err := e.client.CoreV1().Nodes().Get(e.t.Context(), nodeName, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("failed to get node %s: %v", nodeName, err)
	}

	sc, err := e.client.StorageV1().StorageClasses().Get(e.t.Context(), *pvc.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("failed to get storage class %s: %v", *pvc.Spec.StorageClassName, err)
	}

	return controller.ProvisionOptions{
		StorageClass: sc,
		PVName:       "pvc-" + string(pvc.UID),
		PVC:          pvc,
		SelectedNode: node,
	}
}

func (e *testEnv) getPVC(name string) (*corev1.PersistentVolumeClaim, error) {
	return e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(e.t.Context(), name, metav1.GetOptions{})
}

func (e *testEnv) getPV(name string) (*corev1.PersistentVolume, error) {
	return e.client.CoreV1().PersistentVolumes().Get(e.t.Context(), name, metav1.GetOptions{})
}

func (e *testEnv) listPVs() []corev1.PersistentVolume {
	e.t.Helper()

	pvs, err := e.client.CoreV1().PersistentVolumes().List(e.t.Context(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatalf("failed to list PVs: %v", err)
	}

	return pvs.Items
}

// Fixtures.

func testNode(name string, labels map[string]string) *corev1.Node {
	l := map[string]string{corev1.LabelHostname: name}
	maps.Copy(l, labels)

	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

// testCSINode creates a CSINode, drivers are "name" or "name:key1,key2" with topology keys.
func testCSINode(name string, drivers ...string) *storagev1.CSINode {
	csiNode := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: name}}

	for _, d := range drivers {
		driver, keys, _ := strings.Cut(d, ":")

		node := storagev1.CSINodeDriver{Name: driver, NodeID: name}
		if keys != "" {
			node.TopologyKeys = strings.Split(keys, ",")
		}

		csiNode.Spec.Drivers = append(csiNode.Spec.Drivers, node)
	}

	return csiNode
}

func testCSIDriver(name string) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func testStorageClass(name, provisioner string, allowedTopologies ...corev1.TopologySelectorTerm) *storagev1.StorageClass {
	policy := corev1.PersistentVolumeReclaimDelete
	mode := storagev1.VolumeBindingWaitForFirstConsumer

	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: name},
		Provisioner:       provisioner,
		ReclaimPolicy:     &policy,
		VolumeBindingMode: &mode,
		AllowedTopologies: allowedTopologies,
	}
}

func testHybridStorageClass(backends ...string) *storagev1.StorageClass {
	sc := testStorageClass(testHybridClass, DriverName)
	sc.Parameters = map[string]string{paramStorageClasses: strings.Join(backends, ",")}

	return sc
}

func testTopology(key string, values ...string) corev1.TopologySelectorTerm {
	return corev1.TopologySelectorTerm{
		MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: key, Values: values}},
	}
}

// testPVC creates a user PVC of the hybrid StorageClass.
func testPVC(name string) *corev1.PersistentVolumeClaim {
	storageClass := testHybridClass

	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			UID:       types.UID("uid-" + name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

// testBackendPV is the PV that the backend provisioner creates for the helper PVC.
func testBackendPV(helper *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pvc-" + string(helper.UID),
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": sc.Provisioner},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:                   helper.Spec.AccessModes,
			Capacity:                      helper.Spec.Resources.Requests,
			StorageClassName:              sc.Name,
			PersistentVolumeReclaimPolicy: *sc.ReclaimPolicy,
			ClaimRef: &corev1.ObjectReference{
				Kind:       KindPersistentVolumeClaim,
				APIVersion: "v1",
				Namespace:  helper.Namespace,
				Name:       helper.Name,
				UID:        helper.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: sc.Provisioner, VolumeHandle: helper.Name},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}

	if node := helper.Annotations[annotationSelectedNode]; node != "" {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node},
					}},
				}},
			},
		}
	}

	return pv
}

// selectNode sets node-a as the selected node of the user PVC "data", as the scheduler does.
func (e *testEnv) selectNode() {
	e.t.Helper()

	const (
		pvcName  = "data"
		nodeName = "node-a"
	)

	e.inBackend = true
	defer func() { e.inBackend = false }()

	pvc, err := e.getPVC(pvcName)
	if err != nil {
		e.t.Fatalf("failed to get PVC %s: %v", pvcName, err)
	}

	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}

	pvc.Annotations[annotationSelectedNode] = nodeName

	if _, err = e.client.CoreV1().PersistentVolumeClaims(testNamespace).Update(e.t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
		e.t.Fatalf("failed to update PVC %s: %v", pvcName, err)
	}
}

func (e *testEnv) listEvents() []corev1.Event {
	e.t.Helper()

	events, err := e.client.CoreV1().Events("").List(e.t.Context(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatalf("failed to list events: %v", err)
	}

	return events.Items
}

// helperEvent records a Warning event on the helper PVC, as the backend provisioner does.
func (e *testEnv) helperEvent(helper *corev1.PersistentVolumeClaim, eventType, reason, message string) {
	e.t.Helper()

	e.inBackend = true
	defer func() { e.inBackend = false }()

	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%d", helper.Name, len(e.listEvents())), Namespace: helper.Namespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: KindPersistentVolumeClaim, Namespace: helper.Namespace, Name: helper.Name, UID: helper.UID,
		},
		Type:          eventType,
		Reason:        reason,
		Message:       message,
		LastTimestamp: metav1.NewTime(e.clock.Now()),
	}

	if _, err := e.client.CoreV1().Events(helper.Namespace).Create(e.t.Context(), ev, metav1.CreateOptions{}); err != nil {
		e.t.Fatalf("failed to create event: %v", err)
	}
}
