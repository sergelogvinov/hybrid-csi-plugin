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
	"time"

	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"
)

const annBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
const annStorageProvisioner = "volume.kubernetes.io/storage-provisioner"

const (
	// DriverName is the name of the CSI driver
	DriverName = "csi.hybrid.sinextra.dev"
	// DriverVersion is the version of the CSI driver
	DriverVersion = "0.1.0"

	defaultCreateProvisionedPVRetryCount = 5
	defaultCreateProvisionedPVInterval   = 10 * time.Second
)

type HybridProvisioner struct {
	ctx    context.Context
	client kubernetes.Interface

	backoff wait.Backoff

	scLister      storagelistersv1.StorageClassLister
	csiNodeLister storagelistersv1.CSINodeLister
	nodeLister    corelisters.NodeLister
	claimLister   corelisters.PersistentVolumeClaimLister
}

func NewProvisioner(
	ctx context.Context,
	client kubernetes.Interface,
	scLister storagelistersv1.StorageClassLister,
	csiNodeLister storagelistersv1.CSINodeLister,
	nodeLister corelisters.NodeLister,
	claimLister corelisters.PersistentVolumeClaimLister,
) *HybridProvisioner {
	p := &HybridProvisioner{
		ctx:    ctx,
		client: client,

		backoff: wait.Backoff{
			Duration: defaultCreateProvisionedPVInterval,
			Factor:   1, // linear backoff
			Steps:    defaultCreateProvisionedPVRetryCount,
		},

		scLister:      scLister,
		csiNodeLister: csiNodeLister,
		nodeLister:    nodeLister,
		claimLister:   claimLister,
	}

	return p
}

func (p *HybridProvisioner) Provision(ctx context.Context, opts controller.ProvisionOptions) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	klog.V(4).InfoS("Provision: called", "PV", opts.PVName, "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(opts.StorageClass))

	storageClass, err := p.scLister.Get("proxmox")
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	pv, err := p.createPV(ctx, opts, storageClass)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	klog.V(4).InfoS("Provision: persistent volume created", "PV", klog.KObj(pv), "storageClass", pv.Spec.StorageClassName)

	// External provisioner can't update annotation on existence PV, so we need to patch PVC to bind it to the PV.
	err = p.bondPVC(ctx, opts, pv.Name, storageClass)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	pv.ResourceVersion = ""
	return pv, controller.ProvisioningFinished, nil
}

func (p *HybridProvisioner) Delete(_ context.Context, pv *corev1.PersistentVolume) (err error) {
	klog.V(4).InfoS("Delete: called", "pv", pv.Name)

	return nil
}

func (p *HybridProvisioner) createPV(ctx context.Context, opts controller.ProvisionOptions, storageClass *storagev1.StorageClass) (pv *corev1.PersistentVolume, err error) {
	klog.V(4).InfoS("createPV: called", "pvc", klog.KObj(opts.PVC), "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(storageClass))

	var lastSaveError error

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

	// Wait for the pv to be bound to the pvc
	var pvc *corev1.PersistentVolumeClaim

	err = wait.ExponentialBackoff(p.backoff, func() (bool, error) {
		klog.V(4).InfoS("Trying to bind persistent volume", "PVC", klog.KObj(pvcreq), "storageClass", klog.KObj(storageClass))

		if pvc, err = p.claimLister.PersistentVolumeClaims(pvcreq.Namespace).Get(pvcreq.Name); err == nil {
			if pvc.Status.Phase == corev1.ClaimBound && pvc.Spec.VolumeName != "" {
				return true, nil
			}
		}

		lastSaveError = err
		return false, nil
	})
	if err != nil {
		klog.ErrorS(lastSaveError, "Error to bind persistent volume", "pod", klog.KObj(pod), "PVC", klog.KObj(pvcreq), "storageClass", klog.KObj(storageClass))
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

	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"` + corev1.PersistentVolumeReclaimRetain + `"}}`)
	if _, err := p.client.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("failed to patch persistentvolume: %v", err)
	}

	err = wait.ExponentialBackoff(p.backoff, func() (bool, error) {
		klog.V(4).InfoS("Trying to delete persistent volume claim", "PVC", klog.KObj(pvcreq))

		policy := metav1.DeletePropagationForeground
		if err := p.client.CoreV1().PersistentVolumeClaims(pvcreq.Namespace).Delete(ctx, pvcreq.Name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			klog.V(4).ErrorS(err, "Failed to delete persistent volume claim", "PVC", klog.KObj(pvcreq))
			lastSaveError = err

			return false, nil
		}

		return true, nil
	})
	if err != nil {
		klog.ErrorS(lastSaveError, "Error to delete persistentvolumeclaim", "PVC", klog.KObj(pvcreq))
		return nil, err
	}

	patch = []byte(`{"spec":{"claimRef":null}}`)
	pv, err = p.client.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to patch persistentvolume: %v", err)
	}

	return pv, nil
}

func (p *HybridProvisioner) bondPVC(ctx context.Context, opts controller.ProvisionOptions, pvName string, storageClass *storagev1.StorageClass) error {
	patchPVC := corev1.PersistentVolumeClaim{
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
	}

	patch, _ := json.Marshal(&patchPVC)
	if _, err := p.client.CoreV1().PersistentVolumeClaims(opts.PVC.Namespace).Patch(ctx, opts.PVC.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to patch PersistentVolumeClaims: %v", err)
	}

	if storageClass.ReclaimPolicy != nil && *storageClass.ReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"` + corev1.PersistentVolumeReclaimDelete + `"}}`)
		if _, err := p.client.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("failed to patch PersistentVolume: %v", err)
		}
	}

	return nil
}

func bytesToQuantity(bytes int64) resource.Quantity {
	quantity := resource.NewQuantity(bytes, resource.BinarySI)
	return *quantity
}
