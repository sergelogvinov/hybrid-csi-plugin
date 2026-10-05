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
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// HelperName returns the name of the helper PVC for the user PVC.
func HelperName(claim *corev1.PersistentVolumeClaim) string {
	return "pvc-" + string(claim.UID)
}

// claimOwnerReference returns the owner reference of the helper to the claim.
func claimOwnerReference(claim *corev1.PersistentVolumeClaim) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               KindPersistentVolumeClaim,
		Name:               claim.Name,
		UID:                claim.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(false),
	}
}

// buildHelperPVC builds the helper PVC for the backend StorageClass.
//
// The data source of the claim is copied as is, so a restore or a clone is never provisioned as an
// empty volume. The helper is in the namespace of the claim, so the source resolves the same.
// The backend is not chosen by the driver of the source: a backend that cannot use it fails to
// provision the helper, its events are mirrored to the claim and the claim is rescheduled after
// the helper timeout.
func buildHelperPVC(claim *corev1.PersistentVolumeClaim, storageClass *storagev1.StorageClass, node string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HelperName(claim),
			Namespace: claim.Namespace,
			Labels: map[string]string{
				LabelRole: LabelRoleHelper,
			},
			Annotations: map[string]string{
				AnnotationOwnerUID:               string(claim.UID),
				annotationStorageProvisioner:     storageClass.Provisioner,
				annotationBetaStorageProvisioner: storageClass.Provisioner,
				annotationSelectedNode:           node,
			},
			Finalizers:      []string{FinalizerHelper},
			OwnerReferences: []metav1.OwnerReference{claimOwnerReference(claim)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:               claim.Spec.AccessModes,
			StorageClassName:          &storageClass.Name,
			Resources:                 claim.Spec.Resources,
			VolumeMode:                claim.Spec.VolumeMode,
			DataSource:                claim.Spec.DataSource.DeepCopy(),
			DataSourceRef:             claim.Spec.DataSourceRef.DeepCopy(),
			VolumeAttributesClassName: claim.Spec.VolumeAttributesClassName,
		},
	}
}

// isHelperOf reports whether the helper was created by us for the claim.
func isHelperOf(helper, claim *corev1.PersistentVolumeClaim) bool {
	return helper.Annotations[AnnotationOwnerUID] == string(claim.UID)
}

// isLegacyHelperOf reports whether the helper was created by v0.x for the claim: it has no owner-uid,
// uses one of the backend classes and requests the same volume as the claim.
func isLegacyHelperOf(helper, claim *corev1.PersistentVolumeClaim, classes []string) bool {
	if _, ok := helper.Annotations[AnnotationOwnerUID]; ok {
		return false
	}

	if helper.Spec.StorageClassName == nil || !slices.Contains(classes, *helper.Spec.StorageClassName) {
		return false
	}

	volumeMode := func(pvc *corev1.PersistentVolumeClaim) corev1.PersistentVolumeMode {
		return ptr.Deref(pvc.Spec.VolumeMode, corev1.PersistentVolumeFilesystem)
	}

	return slices.Equal(helper.Spec.AccessModes, claim.Spec.AccessModes) &&
		volumeMode(helper) == volumeMode(claim) &&
		helper.Spec.Resources.Requests.Storage().Equal(*claim.Spec.Resources.Requests.Storage())
}

// helperOf returns the helper of the claim from the informer cache, or nil if there is none.
//
// A helper created by v0.x for one of the backend classes is returned too, with legacy set:
// it is not adopted yet. Any other PVC with the helper name is errNotHelper, it is never touched.
func (p *HybridProvisioner) helperOf(claim *corev1.PersistentVolumeClaim, classes []string) (helper *corev1.PersistentVolumeClaim, legacy bool, err error) {
	helper, err = p.claimLister.PersistentVolumeClaims(claim.Namespace).Get(HelperName(claim))
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("failed to get helper persistentvolumeclaim: %v", err)
	}

	switch {
	case isHelperOf(helper, claim):
		return helper, false, nil
	case isLegacyHelperOf(helper, claim, classes):
		return helper, true, nil
	}

	return nil, false, fmt.Errorf("persistentvolumeclaim %s already exists: %w", klog.KObj(helper), errNotHelper)
}

// createHelper creates the helper. An existing helper means the informer cache is behind, the next pass sees it.
func (p *HybridProvisioner) createHelper(ctx context.Context, helper *corev1.PersistentVolumeClaim) error {
	if _, err := p.client.CoreV1().PersistentVolumeClaims(helper.Namespace).Create(ctx, helper, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}

		return fmt.Errorf("failed to create helper persistentvolumeclaim: %v", err)
	}

	return nil
}

// adoptHelper takes over a v0.x helper PVC: adds the metadata a helper gets when we create it.
func (p *HybridProvisioner) adoptHelper(ctx context.Context, helper, claim *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	helper = helper.DeepCopy()

	if helper.Labels == nil {
		helper.Labels = map[string]string{}
	}

	helper.Labels[LabelRole] = LabelRoleHelper

	if helper.Annotations == nil {
		helper.Annotations = map[string]string{}
	}

	helper.Annotations[AnnotationOwnerUID] = string(claim.UID)

	if !slices.Contains(helper.Finalizers, FinalizerHelper) {
		helper.Finalizers = append(helper.Finalizers, FinalizerHelper)
	}

	if metav1.GetControllerOfNoCopy(helper) == nil {
		helper.OwnerReferences = append(helper.OwnerReferences, claimOwnerReference(claim))
	}

	klog.InfoS("Adopting helper persistentvolumeclaim created by a previous version", "PVC", klog.KObj(helper), "claim", klog.KObj(claim))

	helper, err := p.client.CoreV1().PersistentVolumeClaims(helper.Namespace).Update(ctx, helper, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to adopt helper persistentvolumeclaim: %v", err)
	}

	return helper, nil
}

// deleteHelper deletes the helper.
//
// If the helper still owns the volume (the move did not happen), the volume is switched to Delete
// first, so that the backend removes it instead of leaving it Released. The volume is passed if it
// is known, otherwise it is looked up, also when the backend has created it but the helper does not
// point at it yet.
func (p *HybridProvisioner) deleteHelper(ctx context.Context, helper *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	if pv == nil {
		var err error

		if pv, err = p.helperVolume(ctx, helper); err != nil {
			return err
		}
	}

	if pv != nil && boundTo(pv, helper) &&
		pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		if err := p.deleteVolumeWithHelper(ctx, pv, helper); err != nil {
			return err
		}
	}

	if err := p.removeFinalizer(ctx, helper, FinalizerHelper); err != nil {
		return fmt.Errorf("failed to remove finalizer from helper persistentvolumeclaim: %v", err)
	}

	err := p.client.CoreV1().PersistentVolumeClaims(helper.Namespace).Delete(ctx, helper.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &helper.UID},
	})
	if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
		// Conflict: the UID precondition failed, the name belongs to another object, the helper is gone.
		return fmt.Errorf("failed to delete helper persistentvolumeclaim: %v", err)
	}

	// The helper is gone or going, whoever deleted it.
	if _, ok := p.mirrored.LoadAndDelete(helper.UID); ok || err == nil {
		if !helper.CreationTimestamp.IsZero() {
			p.metrics.helperAge.Observe(p.helperAge(helper).Seconds())
		}
	}

	return nil
}

// helperVolume returns the volume of the helper, or nil if there is none: the PV that the helper
// points at, or the PV that points at the helper before the PV controller updates the helper.
func (p *HybridProvisioner) helperVolume(ctx context.Context, helper *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, error) {
	if helper.Spec.VolumeName != "" {
		pv, err := p.client.CoreV1().PersistentVolumes().Get(ctx, helper.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}

			return nil, fmt.Errorf("failed to get persistentvolume: %v", err)
		}

		return pv, nil
	}

	pvs, err := p.volumeLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list persistentvolumes: %v", err)
	}

	for _, pv := range pvs {
		if boundTo(pv, helper) {
			return pv, nil
		}
	}

	return nil, nil
}

// deleteVolumeWithHelper switches the volume to the Delete reclaim policy, if it is still bound to the helper.
func (p *HybridProvisioner) deleteVolumeWithHelper(ctx context.Context, pv *corev1.PersistentVolume, helper *corev1.PersistentVolumeClaim) error {
	patch, err := json.Marshal([]jsonPatchOp{
		{Op: jsonPatchTest, Path: "/spec/claimRef/uid", Value: helper.UID},
		{Op: jsonPatchReplace, Path: "/spec/persistentVolumeReclaimPolicy", Value: corev1.PersistentVolumeReclaimDelete},
	})
	if err != nil {
		return err
	}

	if _, err = p.client.CoreV1().PersistentVolumes().Patch(ctx, pv.Name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		if errors.IsNotFound(err) || errors.IsInvalid(err) {
			// The volume is gone, or it is not bound to the helper any more (the test operation failed).
			return nil
		}

		return fmt.Errorf("failed to update persistentvolume reclaim policy: %v", err)
	}

	return nil
}

// JSON patch operations.
const (
	jsonPatchTest    = "test"
	jsonPatchReplace = "replace"
	jsonPatchRemove  = "remove"
)

// jsonPatchOp is an operation of a JSON patch (RFC 6902).
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// removeFinalizer removes the finalizer from the PVC with a JSON patch.
//
// It does not depend on the resourceVersion: the PV controller updates the helper and the claim
// right after the move, and an update with the object read before would conflict. The patch tests the UID
// and the finalizer at its index, if they changed it is retried with the current object.
func (p *HybridProvisioner) removeFinalizer(ctx context.Context, pvc *corev1.PersistentVolumeClaim, finalizer string) error {
	const attempts = 3

	client := p.client.CoreV1().PersistentVolumeClaims(pvc.Namespace)

	for attempt := 1; ; attempt++ {
		i := slices.Index(pvc.Finalizers, finalizer)
		if i < 0 {
			return nil
		}

		path := "/metadata/finalizers/" + strconv.Itoa(i)

		patch, err := json.Marshal([]jsonPatchOp{
			{Op: jsonPatchTest, Path: "/metadata/uid", Value: pvc.UID},
			{Op: jsonPatchTest, Path: path, Value: finalizer},
			{Op: jsonPatchRemove, Path: path},
		})
		if err != nil {
			return err
		}

		_, err = client.Patch(ctx, pvc.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
		if err == nil || errors.IsNotFound(err) {
			return nil
		}

		if attempt == attempts {
			return err
		}

		uid := pvc.UID

		if pvc, err = client.Get(ctx, pvc.Name, metav1.GetOptions{}); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}

			return err
		}

		if pvc.UID != uid {
			return nil
		}
	}
}

// prepareClaim adds the provisioning finalizer to the claim and pins the backend StorageClass, in one write.
func (p *HybridProvisioner) prepareClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim, backend string) (*corev1.PersistentVolumeClaim, error) {
	annotations := map[string]string{
		AnnotationBackendClass: backend,
		AnnotationHelper:       HelperName(claim),
	}

	if slices.Contains(claim.Finalizers, FinalizerProvisioning) && hasAnnotations(claim.Annotations, annotations) {
		return claim, nil
	}

	claim = claim.DeepCopy()

	if !slices.Contains(claim.Finalizers, FinalizerProvisioning) {
		claim.Finalizers = append(claim.Finalizers, FinalizerProvisioning)
	}

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}

	maps.Copy(claim.Annotations, annotations)

	claim, err := p.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update persistentvolumeclaim: %v", err)
	}

	return claim, nil
}

func hasAnnotations(annotations, want map[string]string) bool {
	for k, v := range want {
		if annotations[k] != v {
			return false
		}
	}

	return true
}
