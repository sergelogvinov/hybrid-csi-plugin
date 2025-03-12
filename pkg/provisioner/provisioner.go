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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
)

const (
	// DriverName is the name of the CSI driver
	DriverName = "csi.hybrid.sinextra.dev"
	// DriverVersion is the version of the CSI driver
	DriverVersion = "0.1.0"

	defaultCreateProvisionedPVRetryCount = 5
	defaultCreateProvisionedPVInterval   = 10 * time.Second

	annBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	annStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	annSelectedNode           = "volume.kubernetes.io/selected-node"
)

const (
	methodDefault    = "auto"
	methodPod        = "pod"
	methodAnnotation = "annotation"
)

// HybridProvisioner is a hybrid provisioner
type HybridProvisioner struct {
	client kubernetes.Interface
	method string

	backoff wait.Backoff

	driverLister  storagelistersv1.CSIDriverLister
	scLister      storagelistersv1.StorageClassLister
	csiNodeLister storagelistersv1.CSINodeLister
	nodeLister    corelisters.NodeLister
	claimLister   corelisters.PersistentVolumeClaimLister
}

// NewProvisioner creates a new hybrid provisioner
func NewProvisioner(
	_ context.Context,
	client kubernetes.Interface,
	method string,
	driverLister storagelistersv1.CSIDriverLister,
	scLister storagelistersv1.StorageClassLister,
	csiNodeLister storagelistersv1.CSINodeLister,
	nodeLister corelisters.NodeLister,
	claimLister corelisters.PersistentVolumeClaimLister,
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

		driverLister:  driverLister,
		scLister:      scLister,
		csiNodeLister: csiNodeLister,
		nodeLister:    nodeLister,
		claimLister:   claimLister,
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

	classes, ok := opts.StorageClass.Parameters["storageClasses"]
	if !ok {
		return nil, controller.ProvisioningFinished, fmt.Errorf("storageClasses parameter is required")
	}

	storageClass, err := p.getStorageClassFromNode(opts.SelectedNode, strings.Split(classes, ","))
	if err != nil {
		return nil, controller.ProvisioningReschedule, err
	}

	var pv *corev1.PersistentVolume

	switch p.method {
	case "auto", "annotation":
		pv, err = p.createPVbyAnnotation(ctx, opts, storageClass)
		if err != nil {
			return nil, controller.ProvisioningFinished, err
		}
	case "pod":
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

// Get first matched StorageClass from the list of storage classes supported by the selected node
func (p *HybridProvisioner) getStorageClassFromNode(selectedNode *corev1.Node, storageClasses []string) (*storagev1.StorageClass, error) {
	selectedCSINode, err := p.csiNodeLister.Get(selectedNode.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting CSINode for selected node %q: %v", selectedNode.Name, err)
	}
	if selectedCSINode == nil {
		return nil, fmt.Errorf("CSINode for selected node %q not found", selectedNode.Name)
	}

	for _, storageClass := range storageClasses {
		class, err := p.scLister.Get(storageClass)
		if err != nil {
			klog.V(4).InfoS("storage class is not found", "node", klog.KObj(selectedNode), "storageClass", storageClass)

			continue
		}

		if len(class.AllowedTopologies) > 0 {
			topologyKeys := getTopologyKeys(selectedCSINode, class.Provisioner)

			selectedTopology, isMissingKey := getTopologyFromNode(selectedNode, topologyKeys)
			if isMissingKey {
				klog.V(5).InfoS("getTopologyFromNode key is missing", "node", klog.KObj(selectedNode), "storageClass", storageClass)

				continue
			}

			allowedTopologiesFlatten := flatten(class.AllowedTopologies)

			found := false
			for _, t := range allowedTopologiesFlatten {
				if t.subset(selectedTopology) {
					found = true
					break
				}
			}

			if !found {
				klog.V(4).InfoS("topology is not in allowed", "node", klog.KObj(selectedNode), "storageClass", storageClass, "topology", selectedTopology)

				continue
			}
		}

		if driver, err := p.driverLister.Get(class.Provisioner); err != nil || driver == nil {
			// Provisioner is not a CSI driver
			return class, nil // nolint: nilerr
		}

		for _, driver := range selectedCSINode.Spec.Drivers {
			if driver.Name == class.Provisioner {
				return class, nil
			}
		}
	}

	return nil, fmt.Errorf("no matching storage class found for selected node %q", selectedNode.Name)
}

func (p *HybridProvisioner) createPVbyAnnotation(ctx context.Context, opts controller.ProvisionOptions, storageClass *storagev1.StorageClass) (pv *corev1.PersistentVolume, err error) {
	klog.V(4).InfoS("createPVusingAnnotation: called", "pvc", klog.KObj(opts.PVC), "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(storageClass))

	pvcreq := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.PVName,
			Namespace: opts.PVC.Namespace,
			Annotations: map[string]string{
				annStorageProvisioner:     storageClass.Provisioner,
				annBetaStorageProvisioner: storageClass.Provisioner,
				annSelectedNode:           opts.SelectedNode.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      opts.PVC.Spec.AccessModes,
			StorageClassName: &storageClass.Name,
			Resources:        opts.PVC.Spec.Resources,
			VolumeMode:       opts.PVC.Spec.VolumeMode,
		},
	}

	if _, err := p.claimLister.PersistentVolumeClaims(pvcreq.Namespace).Get(pvcreq.Name); err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get persistentvolumeclaim: %v", err)
		}

		_, err = p.client.CoreV1().PersistentVolumeClaims(pvcreq.Namespace).Create(ctx, pvcreq, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create persistentvolumeclaim: %v", err)
		}
	}

	var pvc *corev1.PersistentVolumeClaim

	// Wait for the PV to be bound to the PVC
	pvc, err = p.waitBindPVC(ctx, pvcreq)
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

	// External provisioner can't update annotation on existence PV, so we need to patch PVC to bind it to the PV.
	err = p.bondPVC(ctx, opts, pv.Name, storageClass)
	if err != nil {
		return nil, err
	}

	return pv, nil
}

func (p *HybridProvisioner) createPVbyPOD(ctx context.Context, opts controller.ProvisionOptions, storageClass *storagev1.StorageClass) (pv *corev1.PersistentVolume, err error) {
	klog.V(4).InfoS("createPVusingPOD: called", "pvc", klog.KObj(opts.PVC), "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(storageClass))

	pvcreq := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.PVName,
			Namespace: opts.PVC.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      opts.PVC.Spec.AccessModes,
			StorageClassName: &storageClass.Name,
			Resources:        opts.PVC.Spec.Resources,
			VolumeMode:       opts.PVC.Spec.VolumeMode,
		},
	}

	if _, err := p.claimLister.PersistentVolumeClaims(pvcreq.Namespace).Get(pvcreq.Name); err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get persistentvolumeclaim: %v", err)
		}

		_, err = p.client.CoreV1().PersistentVolumeClaims(pvcreq.Namespace).Create(ctx, pvcreq, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create persistentvolumeclaim: %v", err)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("provisioner-%s", opts.PVName),
			Namespace: pvcreq.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "provisioner",
					Image: "registry.k8s.io/pause:3.10",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("10Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("10Mi"),
						},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			NodeSelector: map[string]string{
				corev1.LabelHostname: opts.SelectedNode.Name,
			},
			Volumes: []corev1.Volume{
				{
					Name:         "provisioner",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcreq.Name}},
				},
			},
		},
	}

	pod, err = p.client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	var (
		pvc           *corev1.PersistentVolumeClaim
		lastSaveError error
	)

	// Wait for the pv to be bound to the pvc
	pvc, err = p.waitBindPVC(ctx, pvcreq)
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

	// External provisioner can't update annotation on existence PV, so we need to patch PVC to bind it to the PV.
	err = p.bondPVC(ctx, opts, pv.Name, storageClass)
	if err != nil {
		return nil, err
	}

	return pv, nil
}

func (p *HybridProvisioner) bondPVC(ctx context.Context, opts controller.ProvisionOptions, pvName string, storageClass *storagev1.StorageClass) error {
	patch, _ := json.Marshal(&corev1.PersistentVolumeClaim{ // nolint: errcheck,errchkjson
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				annStorageProvisioner:       storageClass.Provisioner,
				annBetaStorageProvisioner:   storageClass.Provisioner,
				volume.AnnBindCompleted:     "yes",
				volume.AnnBoundByController: "yes",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: pvName,
		},
	})

	if _, err := p.client.CoreV1().PersistentVolumeClaims(opts.PVC.Namespace).Patch(ctx, opts.PVC.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to patch PersistentVolumeClaims: %v", err)
	}

	if storageClass.ReclaimPolicy != nil && *storageClass.ReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		patch := fmt.Sprintf(
			`{
				"spec":
				{
					"persistentVolumeReclaimPolicy":"%s",
					"claimRef":
					{
						"apiVersion":"%s",
						"kind":"%s",
						"name":"%s",
						"namespace":"%s",
						"uid":"%s"
					}
				}
			}`,
			corev1.PersistentVolumeReclaimDelete,
			opts.PVC.APIVersion,
			opts.PVC.Kind,
			opts.PVC.Name,
			opts.PVC.Namespace,
			opts.PVC.UID,
		)
		if _, err := p.client.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("failed to patch PersistentVolume: %v", err)
		}
	}

	return nil
}

func (p *HybridProvisioner) waitBindPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	watcher, err := p.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + pvc.Name,
	})
	if err != nil {
		return nil, err
	}

	defer watcher.Stop()

	timeout := time.After(time.Second * 30)

	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch channel closed unexpectedly")
			}

			if event.Type == watch.Modified {
				obj, ok := event.Object.(*corev1.PersistentVolumeClaim)
				if ok && obj.Status.Phase == corev1.ClaimBound {
					return obj, nil
				}
			}

		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for PersistentVolumeClaims %s to be boned", pvc.Name)
		}
	}
}

func (p *HybridProvisioner) releasePV(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (pv *corev1.PersistentVolume, err error) {
	var lastSaveError error
	var newFinalizers []string
	var patchStr string

	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"` + corev1.PersistentVolumeReclaimRetain + `"}}`)
	if _, err := p.client.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("failed to patch persistentvolume: %v", err)
	}

	for _, f := range pvc.Finalizers {
		// Remove kubernetes.io/pvc-protection to avoid PV-controller to rebind PV to Terminating PVC usec for provisioning.
		if f != "kubernetes.io/pvc-protection" {
			newFinalizers = append(newFinalizers, f)
		}
	}
	if len(newFinalizers) > 0 {
		patchStr = fmt.Sprintf(`{"metadata": {"finalizers": ["%s"]}}`, strings.Join(newFinalizers, `", "`))
	} else {
		patchStr = `{"metadata":{"finalizers":null}}`
	}
	if _, err := p.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(ctx, pvc.Name, types.MergePatchType, []byte(patchStr), metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("failed to remove finalizer from persistentvolumeClaim: %v", err)
	}

	err = wait.ExponentialBackoff(p.backoff, func() (bool, error) {
		klog.V(4).InfoS("Trying to delete persistent volume claim", "PVC", klog.KObj(pvc))

		policy := metav1.DeletePropagationForeground
		if err := p.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			klog.V(4).ErrorS(err, "Failed to delete persistent volume claim", "PVC", klog.KObj(pvc))
			lastSaveError = err

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		klog.ErrorS(lastSaveError, "Error to delete persistentvolumeclaim", "PVC", klog.KObj(pvc))
		return nil, err
	}

	patch = []byte(`{"spec":{"claimRef":null}}`)
	pv, err = p.client.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to patch persistentvolume: %v", err)
	}

	return pv, nil
}
