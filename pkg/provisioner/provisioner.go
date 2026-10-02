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
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog/v2"
)

const (
	defaultCreateProvisionedPVRetryCount = 5
	defaultCreateProvisionedPVInterval   = 10 * time.Second
)

// Listers is the set of informer listers used by the provisioner.
type Listers struct {
	CSIDrivers     storagelistersv1.CSIDriverLister
	StorageClasses storagelistersv1.StorageClassLister
	CSINodes       storagelistersv1.CSINodeLister
	Nodes          corelisters.NodeLister
	Claims         corelisters.PersistentVolumeClaimLister
	Volumes        corelisters.PersistentVolumeLister
	Namespaces     corelisters.NamespaceLister
}

// NewListers creates the listers from the shared informer factory.
// The informers are registered in the factory, so it has to be started after this call.
func NewListers(factory informers.SharedInformerFactory) Listers {
	return Listers{
		CSIDrivers:     factory.Storage().V1().CSIDrivers().Lister(),
		StorageClasses: factory.Storage().V1().StorageClasses().Lister(),
		CSINodes:       factory.Storage().V1().CSINodes().Lister(),
		Nodes:          factory.Core().V1().Nodes().Lister(),
		Claims:         factory.Core().V1().PersistentVolumeClaims().Lister(),
		Volumes:        factory.Core().V1().PersistentVolumes().Lister(),
		Namespaces:     factory.Core().V1().Namespaces().Lister(),
	}
}

// HybridProvisioner is a hybrid provisioner
type HybridProvisioner struct {
	client kubernetes.Interface
	method string

	backoff     wait.Backoff
	bindTimeout time.Duration

	driverLister    storagelistersv1.CSIDriverLister
	scLister        storagelistersv1.StorageClassLister
	csiNodeLister   storagelistersv1.CSINodeLister
	nodeLister      corelisters.NodeLister
	claimLister     corelisters.PersistentVolumeClaimLister
	volumeLister    corelisters.PersistentVolumeLister
	namespaceLister corelisters.NamespaceLister
}

// NewProvisioner creates a new hybrid provisioner
func NewProvisioner(
	_ context.Context,
	client kubernetes.Interface,
	method string,
	listers Listers,
) *HybridProvisioner {
	switch method {
	case methodDefault, methodPod, methodAnnotation:
	default:
		method = methodDefault
		klog.Warningf("Unknown provisioner method, using %s", method)
	}

	p := &HybridProvisioner{
		client: client,

		method: method,

		backoff: wait.Backoff{
			Duration: defaultCreateProvisionedPVInterval,
			Factor:   1, // linear backoff
			Steps:    defaultCreateProvisionedPVRetryCount,
		},
		bindTimeout: helperBindTimeout,

		driverLister:    listers.CSIDrivers,
		scLister:        listers.StorageClasses,
		csiNodeLister:   listers.CSINodes,
		nodeLister:      listers.Nodes,
		claimLister:     listers.Claims,
		volumeLister:    listers.Volumes,
		namespaceLister: listers.Namespaces,
	}

	return p
}

// Provision creates a volume i.e. the storage asset and returns a PV object
// for the volume. The provisioner can return an error (e.g. timeout) and state
// ProvisioningInBackground to tell the controller that provisioning may be in
// progress after Provision() finishes. The controller will call Provision()
// again with the same parameters, assuming that the provisioner continues
// provisioning the volume. The provisioner must return either final error (with
// ProvisioningFinished) or success eventually, otherwise the controller will try
// forever (unless FailedProvisionThreshold is set).
func (p *HybridProvisioner) Provision(ctx context.Context, opts controller.ProvisionOptions) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	klog.V(4).InfoS("Provision: called", "PV", opts.PVName, "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(opts.StorageClass))

	if opts.StorageClass == nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("storageClass is required")
	}

	classes, err := backendClasses(opts.StorageClass)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	if opts.SelectedNode == nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("selected node is required, use WaitForFirstConsumer volume binding mode")
	}

	storageClass, err := p.selectBackend(opts.SelectedNode, classes)
	if err != nil {
		return nil, controller.ProvisioningReschedule, err
	}

	var pv *corev1.PersistentVolume

	switch p.method {
	case methodDefault, methodAnnotation:
		pv, err = p.createPVbyAnnotation(ctx, opts, storageClass)
		if err != nil {
			return nil, controller.ProvisioningFinished, err
		}
	case methodPod:
		pv, err = p.createPVbyPOD(ctx, opts, storageClass)
		if err != nil {
			return nil, controller.ProvisioningFinished, err
		}
	}

	pv.ResourceVersion = ""

	return pv, controller.ProvisioningFinished, nil
}

// Delete removes the storage asset that was created by Provision backing the
// given PV. Does not delete the PV object itself.
func (p *HybridProvisioner) Delete(_ context.Context, pv *corev1.PersistentVolume) (err error) {
	klog.V(4).InfoS("Delete: called", "pv", pv.Name)

	return nil
}

func (p *HybridProvisioner) createPVbyAnnotation(ctx context.Context, opts controller.ProvisionOptions, storageClass *storagev1.StorageClass) (pv *corev1.PersistentVolume, err error) {
	klog.V(4).InfoS("createPVusingAnnotation: called", "pvc", klog.KObj(opts.PVC), "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(storageClass))

	pvcreq := buildHelperPVC(opts, storageClass, true)

	if err = p.ensureHelperPVC(ctx, pvcreq); err != nil {
		return nil, err
	}

	// Wait for the PV to be bound to the PVC
	pvc, err := p.waitBindPVC(ctx, pvcreq)
	if err != nil {
		klog.ErrorS(err, "Error to bind persistent volume", "PVC", klog.KObj(pvcreq), "storageClass", klog.KObj(storageClass))
		return nil, err
	}

	pv, err = p.releasePV(ctx, pvc)
	if err != nil {
		klog.ErrorS(err, "Error to release persistent volume", "PVC", klog.KObj(pvc), "storageClass", klog.KObj(storageClass))
		return nil, err
	}

	klog.V(4).InfoS("Provision: persistent volume created", "PV", klog.KObj(pv), "storageClass", pv.Spec.StorageClassName)

	err = p.bondPVC(ctx, opts, pv.Name, storageClass)
	if err != nil {
		return nil, err
	}

	return pv, nil
}

func (p *HybridProvisioner) createPVbyPOD(ctx context.Context, opts controller.ProvisionOptions, storageClass *storagev1.StorageClass) (pv *corev1.PersistentVolume, err error) {
	klog.V(4).InfoS("createPVusingPOD: called", "pvc", klog.KObj(opts.PVC), "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(storageClass))

	pvcreq := buildHelperPVC(opts, storageClass, false)

	if err = p.ensureHelperPVC(ctx, pvcreq); err != nil {
		return nil, err
	}

	pod, err := p.client.CoreV1().Pods(pvcreq.Namespace).Create(ctx, buildHelperPod(opts, pvcreq), metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	var lastSaveError error

	// Wait for the pv to be bound to the pvc
	pvc, err := p.waitBindPVC(ctx, pvcreq)
	if err != nil {
		klog.ErrorS(err, "Error to bind persistent volume", "pod", klog.KObj(pod), "PVC", klog.KObj(pvcreq), "storageClass", klog.KObj(storageClass))
		return nil, err
	}

	/// Now, we have pod + pvc + pv

	err = wait.ExponentialBackoff(p.backoff, func() (bool, error) {
		klog.V(4).InfoS("Trying to delete pod", "pod", klog.KObj(pod))

		if err = p.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err == nil || errors.IsNotFound(err) {
			return true, nil
		}

		klog.V(4).ErrorS(err, "Failed to delete pod", "pod", klog.KObj(pod))
		lastSaveError = err

		return false, nil
	})
	if err != nil {
		klog.ErrorS(lastSaveError, "Error to delete pod", "pod", klog.KObj(pod))
		return nil, err
	}

	pv, err = p.releasePV(ctx, pvc)
	if err != nil {
		klog.ErrorS(err, "Error to release persistent volume", "PVC", klog.KObj(pvc), "storageClass", klog.KObj(storageClass))
		return nil, err
	}

	klog.V(4).InfoS("Provision: persistent volume created", "PV", klog.KObj(pv), "storageClass", pv.Spec.StorageClassName)

	err = p.bondPVC(ctx, opts, pv.Name, storageClass)
	if err != nil {
		return nil, err
	}

	return pv, nil
}
