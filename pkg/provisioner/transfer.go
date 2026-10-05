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
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
)

// errForeignVolume is returned when the volume is bound to neither the helper nor the claim, it is never moved.
var errForeignVolume = errors.New("persistentvolume is bound to another claim")

// Fault injection points, see HybridProvisioner.SetFaultHook.
const (
	// FaultAfterMove is right after the move: the volume is moved to the claim, the claim is not bound yet.
	FaultAfterMove = "after-move"
	// FaultAfterBind is right after the bind: the claim is bound, the helper and the claim finalizer are left.
	FaultAfterBind = "after-bind"
)

// moveVolume moves the volume from the helper to the claim and binds the claim to it.
// It returns errForeignVolume if the volume is bound to neither the helper nor the claim.
func (p *HybridProvisioner) moveVolume(
	ctx context.Context,
	hsc *storagev1.StorageClass,
	claim, helper *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim, error) {
	provisioner := pv.Annotations[volume.AnnDynamicallyProvisioned]
	if provisioner == "" {
		provisioner = helper.Annotations[annotationStorageProvisioner]
	}

	moved := boundTo(pv, claim)

	pv, err := p.transferVolume(ctx, pv, helper, claim, hsc)
	if err != nil {
		p.metrics.phase(phaseMove, err)

		return nil, nil, err
	}

	if !moved {
		p.fault(FaultAfterMove)
	}

	bound := claim.Spec.VolumeName == pv.Name

	claim, err = p.bindClaim(ctx, claim, pv, provisioner)
	p.metrics.phase(phaseMove, err)

	if err != nil {
		return nil, nil, err
	}

	if !bound {
		p.fault(FaultAfterBind)
	}

	return pv, claim, nil
}

// cleanup finishes provisioning of the claim: deletes the helper, if there is one,
// then removes the provisioning finalizer from the claim. The volume is passed if it is known.
func (p *HybridProvisioner) cleanup(
	ctx context.Context,
	claim, helper *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) (err error) {
	defer func() { p.metrics.phase(phaseCleanup, err) }()

	if helper != nil {
		if err = p.deleteHelper(ctx, helper, pv); err != nil {
			return err
		}
	}

	return p.releaseClaim(ctx, claim)
}

// boundTo reports whether the volume is bound to the PVC, by UID: the helper before the move, the claim after it.
func boundTo(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) bool {
	return pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == pvc.UID
}

// ReclaimPolicy returns the reclaim policy of the hybrid StorageClass, Delete if it has none.
func ReclaimPolicy(hsc *storagev1.StorageClass) corev1.PersistentVolumeReclaimPolicy {
	if hsc.ReclaimPolicy != nil {
		return *hsc.ReclaimPolicy
	}

	return corev1.PersistentVolumeReclaimDelete
}

// transferVolume moves the volume from the helper to the claim in one write: claimRef, reclaim policy
// and hybrid metadata. There is no moment when the volume is Available, so the PV controller cannot bind it to another claim.
func (p *HybridProvisioner) transferVolume(
	ctx context.Context,
	pv *corev1.PersistentVolume,
	helper, claim *corev1.PersistentVolumeClaim,
	hsc *storagev1.StorageClass,
) (*corev1.PersistentVolume, error) {
	switch {
	case boundTo(pv, claim):
		return pv, nil
	case !boundTo(pv, helper):
		return nil, fmt.Errorf("%w: persistentvolume %s, claimRef %v", errForeignVolume, pv.Name, pv.Spec.ClaimRef)
	}

	pv = pv.DeepCopy()
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       KindPersistentVolumeClaim,
		Namespace:  claim.Namespace,
		Name:       claim.Name,
		UID:        claim.UID,
	}
	pv.Spec.PersistentVolumeReclaimPolicy = ReclaimPolicy(hsc)

	if pv.Labels == nil {
		pv.Labels = map[string]string{}
	}

	pv.Labels[LabelManaged] = ValueTrue

	if pv.Annotations == nil {
		pv.Annotations = map[string]string{}
	}

	pv.Annotations[AnnotationClaim] = claim.Namespace + "/" + claim.Name
	pv.Annotations[AnnotationStorageClass] = hsc.Name

	pv, err := p.client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update persistentvolume: %v", err)
	}

	klog.V(4).InfoS("Persistent volume moved to the claim", "PV", klog.KObj(pv), "claim", klog.KObj(claim))

	return pv, nil
}

// bindClaim points the claim at the volume.
//
// The claim and the volume have different storage classes, bind-completed makes the PV controller
// take the syncBoundClaim path, which does not compare them.
//
// After the move the volume is pre-bound to the claim, and the PV controller may bind the claim
// itself first: the update then conflicts, and the claim is read again. It is bound to the volume, but without the
// provisioner annotations, so they are still written.
func (p *HybridProvisioner) bindClaim(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	provisioner string,
) (*corev1.PersistentVolumeClaim, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if claim.Spec.VolumeName != "" && claim.Spec.VolumeName != pv.Name {
			return fmt.Errorf("persistentvolumeclaim is bound to another persistentvolume %s", claim.Spec.VolumeName)
		}

		update := claim.DeepCopy()
		update.Spec.VolumeName = pv.Name

		if update.Annotations == nil {
			update.Annotations = map[string]string{}
		}

		update.Annotations[volume.AnnBindCompleted] = valueYes
		update.Annotations[volume.AnnBoundByController] = valueYes
		update.Annotations[annotationStorageProvisioner] = provisioner
		update.Annotations[annotationBetaStorageProvisioner] = provisioner

		if claim.Spec.VolumeName == pv.Name && maps.Equal(claim.Annotations, update.Annotations) {
			return nil
		}

		updated, err := p.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(ctx, update, metav1.UpdateOptions{})
		if err == nil {
			claim = updated

			return nil
		}

		if !apierrors.IsConflict(err) {
			return err
		}

		fresh, getErr := p.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}

		if fresh.UID != claim.UID {
			return fmt.Errorf("persistentvolumeclaim is recreated, uid %s", fresh.UID)
		}

		claim = fresh

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to bind persistentvolumeclaim: %w", err)
	}

	return claim, nil
}

// releaseClaim removes the provisioning finalizer from the claim.
func (p *HybridProvisioner) releaseClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	if err := p.removeFinalizer(ctx, claim, FinalizerProvisioning); err != nil {
		return fmt.Errorf("failed to remove finalizer from persistentvolumeclaim: %v", err)
	}

	return nil
}
