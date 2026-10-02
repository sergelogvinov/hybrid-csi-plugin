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
	"maps"
	"strings"
	"testing"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testNamespace   = "default"
	testHybridClass = "hybrid"
)

// testEnv is a fake cluster: fake clientset, started informers and the provisioner under test.
type testEnv struct {
	t      *testing.T
	client *fake.Clientset
	prov   *HybridProvisioner
}

// newTestEnv creates the fake cluster with the objects and the provisioner.
func newTestEnv(t *testing.T, method string, objs ...runtime.Object) *testEnv {
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

	prov := NewProvisioner(ctx, client, method, listers)
	prov.bindTimeout = 5 * time.Second
	prov.backoff = wait.Backoff{Duration: 10 * time.Millisecond, Factor: 1, Steps: 3}

	return &testEnv{t: t, client: client, prov: prov}
}

// simulateBackend emulates the backend provisioner and the PV controller:
// once the provisioner watches a helper PVC, a PV is created and bound to it.
func (e *testEnv) simulateBackend() {
	e.client.PrependWatchReactor("persistentvolumeclaims", func(action clienttesting.Action) (bool, watch.Interface, error) {
		wa, ok := action.(clienttesting.WatchActionImpl)
		if !ok {
			return false, nil, nil
		}

		name, ok := wa.GetWatchRestrictions().Fields.RequiresExactMatch("metadata.name")
		if !ok {
			return false, nil, nil
		}

		gvr := action.GetResource()
		ns := action.GetNamespace()

		w, err := e.client.Tracker().Watch(gvr, ns, wa.GetListOptions())
		if err != nil {
			return true, nil, err
		}

		go e.bindHelper(ns, name)

		return true, w, nil
	})
}

// bindHelper creates a backend PV for the helper PVC and binds them.
func (e *testEnv) bindHelper(namespace, name string) {
	pvcs := e.client.CoreV1().PersistentVolumeClaims(namespace)

	helper, err := pvcs.Get(e.t.Context(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Errorf("backend: failed to get helper PVC %s/%s: %v", namespace, name, err)

		return
	}

	sc, err := e.client.StorageV1().StorageClasses().Get(e.t.Context(), *helper.Spec.StorageClassName, metav1.GetOptions{})
	if err != nil {
		e.t.Errorf("backend: failed to get storage class %s: %v", *helper.Spec.StorageClassName, err)

		return
	}

	pv := testBackendPV(helper, sc)

	if _, err = e.client.CoreV1().PersistentVolumes().Create(e.t.Context(), pv, metav1.CreateOptions{}); err != nil {
		e.t.Errorf("backend: failed to create PV %s: %v", pv.Name, err)

		return
	}

	helper.Spec.VolumeName = pv.Name
	helper.Status.Phase = corev1.ClaimBound
	helper.Finalizers = append(helper.Finalizers, finalizerPVCProtection)

	if helper, err = pvcs.Update(e.t.Context(), helper, metav1.UpdateOptions{}); err != nil {
		e.t.Errorf("backend: failed to update helper PVC: %v", err)

		return
	}

	if helper.Status.Phase != corev1.ClaimBound {
		helper.Status.Phase = corev1.ClaimBound

		if _, err = pvcs.UpdateStatus(e.t.Context(), helper, metav1.UpdateOptions{}); err != nil {
			e.t.Errorf("backend: failed to update helper PVC status: %v", err)
		}
	}
}

// provisionUntilDone calls Provision() until it stops returning ProvisioningInBackground.
func (e *testEnv) provisionUntilDone(opts controller.ProvisionOptions, maxPasses int) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	e.t.Helper()

	var (
		pv    *corev1.PersistentVolume
		state controller.ProvisioningState
		err   error
	)

	for range maxPasses {
		pv, state, err = e.prov.Provision(e.t.Context(), opts)
		if state != controller.ProvisioningInBackground {
			return pv, state, err
		}
	}

	e.t.Fatalf("Provision() did not converge in %d passes, last error: %v", maxPasses, err)

	return nil, state, err
}

// provisionOptions builds the options the library passes to Provision() for the user PVC.
func (e *testEnv) provisionOptions(pvcName, nodeName string) controller.ProvisionOptions {
	e.t.Helper()

	pvc, err := e.client.CoreV1().PersistentVolumeClaims(testNamespace).Get(e.t.Context(), pvcName, metav1.GetOptions{})
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

func testHybridStorageClass(name string, backends ...string) *storagev1.StorageClass {
	sc := testStorageClass(name, DriverName)
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
			Name:        fmt.Sprintf("backend-%s", helper.Name),
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": sc.Provisioner},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:                   helper.Spec.AccessModes,
			Capacity:                      helper.Spec.Resources.Requests,
			StorageClassName:              sc.Name,
			PersistentVolumeReclaimPolicy: *sc.ReclaimPolicy,
			ClaimRef: &corev1.ObjectReference{
				Kind:       "PersistentVolumeClaim",
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

	if node := helper.Annotations[annSelectedNode]; node != "" {
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
