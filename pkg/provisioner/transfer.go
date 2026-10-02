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

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
)

// Moving the backend PV (P) from the helper PVC (H) to the user PVC (U).

// releasePV detaches the backend PV from the helper PVC: it switches the PV to Retain,
// deletes the helper PVC and clears the PV claimRef.
func (p *HybridProvisioner) releasePV(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (pv *corev1.PersistentVolume, err error) {
	var (
		lastSaveError error
		newFinalizers []string
		patchStr      string
	)

	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"` + corev1.PersistentVolumeReclaimRetain + `"}}`)
	if _, err := p.client.CoreV1().PersistentVolumes().Patch(ctx, pvc.Spec.VolumeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("failed to patch persistentvolume: %v", err)
	}

	for _, f := range pvc.Finalizers {
		// Remove kubernetes.io/pvc-protection to avoid PV-controller to rebind PV to Terminating PVC usec for provisioning.
		if f != finalizerPVCProtection {
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

// bondPVC binds the user PVC to the released backend PV.
//
// External provisioner can't update annotation on existence PV, so we need to patch PVC to bind it to the PV.
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
