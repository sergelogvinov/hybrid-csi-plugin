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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Reconcile handles a user PVC or a helper PVC in the cases Provision does not see: the user PVC
// is deleted during provisioning, the helper is deleted by someone else, or a bound user PVC still
// has the leftovers of a cleanup interrupted by a crash.
//
// A PVC that Provision still owns is left alone. The leftovers of a bound user PVC are cleaned up
// only after the cleanup delay (DefaultCleanupDelay), the Provision() pass that has just bound it
// does it itself: until then Reconcile returns how long to wait before it is called again for the PVC.
func (p *HybridProvisioner) Reconcile(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (time.Duration, error) {
	if pvc.Labels[LabelRole] == LabelRoleHelper {
		return p.reconcileHelper(ctx, pvc)
	}

	return p.reconcileClaim(ctx, pvc)
}

// reconcileClaim handles a user claim.
func (p *HybridProvisioner) reconcileClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) (time.Duration, error) {
	switch {
	case claim.DeletionTimestamp != nil:
		p.boundSince.Delete(claim.UID)

		if !slices.Contains(claim.Finalizers, FinalizerProvisioning) {
			return 0, nil
		}

		return 0, p.cancelClaim(ctx, claim)
	case p.isBound(claim):
		return p.cleanupBound(ctx, claim)
	}

	// The claim is being provisioned, Provision() owns it.
	return 0, nil
}

// cancelClaim releases a claim that is deleted before it is bound, or after a crash before the cleanup is finished.
func (p *HybridProvisioner) cancelClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	helper, err := p.ownHelperOf(claim)
	if err != nil {
		return err
	}

	if err = p.cleanup(ctx, claim, helper, nil); err != nil {
		return err
	}

	if helper != nil && claim.Spec.VolumeName == "" {
		p.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonProvisioningCanceled,
			"Claim is deleted during provisioning, helper %s is deleted", helper.Name)
	}

	klog.V(2).InfoS("Claim is released", "claim", klog.KObj(claim), "helper", klog.KObj(helper))

	return nil
}

// cleanupBound removes the leftovers of an interrupted cleanup of a bound claim, after the cleanup delay.
func (p *HybridProvisioner) cleanupBound(ctx context.Context, claim *corev1.PersistentVolumeClaim) (time.Duration, error) {
	helper, err := p.ownHelperOf(claim)
	if err != nil {
		return 0, err
	}

	if helper == nil && !slices.Contains(claim.Finalizers, FinalizerProvisioning) {
		p.boundSince.Delete(claim.UID)

		return 0, nil
	}

	now := p.clock.Now()
	since, _ := p.boundSince.LoadOrStore(claim.UID, now)

	if wait := p.cleanupDelay - now.Sub(since.(time.Time)); wait > 0 {
		return wait, nil
	}

	if err = p.cleanup(ctx, claim, helper, nil); err != nil {
		return 0, err
	}

	p.boundSince.Delete(claim.UID)
	p.recorder.Event(claim, corev1.EventTypeNormal, ReasonCleanupFinished, "Provisioning cleanup is finished")
	klog.V(2).InfoS("Provisioning cleanup is finished", "claim", klog.KObj(claim), "helper", klog.KObj(helper))

	return 0, nil
}

// reconcileHelper handles a helper.
func (p *HybridProvisioner) reconcileHelper(ctx context.Context, helper *corev1.PersistentVolumeClaim) (time.Duration, error) {
	if helper.Annotations[AnnotationOwnerUID] == "" {
		return 0, nil
	}

	claim, err := p.ownerOf(helper)
	if err != nil {
		return 0, err
	}

	if helper.DeletionTimestamp != nil {
		if !slices.Contains(helper.Finalizers, FinalizerHelper) {
			return 0, nil
		}

		return 0, p.settleDeletedHelper(ctx, claim, helper)
	}

	if claim != nil && p.isBound(claim) {
		// The volume is moved, the helper is a leftover.
		return p.cleanupBound(ctx, claim)
	}

	// The claim is gone: the garbage collector deletes the helper by its owner reference, then it is handled above.
	return 0, nil
}

// settleDeletedHelper handles a helper deleted by someone else: it is released, or its volume is recovered.
func (p *HybridProvisioner) settleDeletedHelper(ctx context.Context, claim, helper *corev1.PersistentVolumeClaim) error {
	switch {
	case claim == nil || claim.DeletionTimestamp != nil:
		// The claim is gone, the fresh volume is deleted with the helper.
		if err := p.deleteHelper(ctx, helper, nil); err != nil {
			return err
		}

		klog.V(2).InfoS("Helper of a deleted claim is released", "helper", klog.KObj(helper))
	case helper.Spec.VolumeName != "":
		// The helper is bound, the volume is kept: finish the move.
		return p.recoverVolume(ctx, claim, helper)
	default:
		// The helper is pending, the next Provision() pass creates a new one.
		if err := p.deleteHelper(ctx, helper, nil); err != nil {
			return err
		}

		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonHelperDeleted,
			"Helper %s is deleted before the volume was provisioned, it is created again", helper.Name)
		klog.V(2).InfoS("Pending helper is deleted by someone else", "claim", klog.KObj(claim), "helper", klog.KObj(helper))
	}

	return nil
}

// recoverVolume moves the volume of the deleted helper to the claim.
func (p *HybridProvisioner) recoverVolume(ctx context.Context, claim, helper *corev1.PersistentVolumeClaim) error {
	pv, err := p.volumeLister.Get(helper.Spec.VolumeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return p.deleteHelper(ctx, helper, nil)
		}

		return err
	}

	// As in Provision(), a volume that does not fit the selected node is discarded, the next pass creates a new helper.
	if reason := p.volumeFitsSelectedNode(claim, pv); reason != nil {
		if err = p.deleteHelper(ctx, helper, pv); err != nil {
			return err
		}

		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonHelperDeleted,
			"Helper %s is deleted by someone else, its volume %s is not used: %v; the helper is created again", helper.Name, pv.Name, reason)
		klog.V(2).InfoS("Volume of a deleted helper does not fit the claim, it is discarded", "claim", klog.KObj(claim), "helper", klog.KObj(helper), "PV", klog.KObj(pv), "reason", reason)

		return nil
	}

	hsc, err := p.hybridClassOf(claim)
	if err != nil {
		return err
	}

	pv, claim, err = p.moveVolume(ctx, hsc, claim, helper, pv)
	if err != nil {
		if errors.Is(err, errForeignVolume) {
			// The volume is not ours, the helper is released without touching it.
			klog.InfoS("Helper volume is bound to another claim, it is not moved", "helper", klog.KObj(helper), "error", err)

			return p.deleteHelper(ctx, helper, nil)
		}

		return err
	}

	if err = p.cleanup(ctx, claim, helper, pv); err != nil {
		return err
	}

	p.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonVolumeRecovered,
		"Helper %s is deleted by someone else, volume %s is moved to the claim", helper.Name, pv.Name)
	klog.V(2).InfoS("Volume of a deleted helper is moved to the claim", "claim", klog.KObj(claim), "helper", klog.KObj(helper), "PV", klog.KObj(pv))

	return nil
}

// volumeFitsSelectedNode checks the volume against the node selected for the claim, see volumeFits.
func (p *HybridProvisioner) volumeFitsSelectedNode(claim *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	// The claim may have no selected node any more after the move, the volume is its own.
	if boundTo(pv, claim) {
		return nil
	}

	name := claim.Annotations[annotationSelectedNode]
	if name == "" {
		return fmt.Errorf("persistentvolumeclaim %s has no selected node", klog.KObj(claim))
	}

	node, err := p.nodeLister.Get(name)
	if err != nil {
		return fmt.Errorf("failed to get node %q: %v", name, err)
	}

	return volumeFits(claim, pv, node)
}

// ownHelperOf returns the helper of the claim, or nil if there is none or the PVC with
// its name is not ours. A helper created by v0.x and not adopted yet is returned too:
// the claim may be deleted before Provision() adopts it.
func (p *HybridProvisioner) ownHelperOf(claim *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	classes, err := p.backendClassesOf(claim)
	if err != nil {
		// Without the backend classes a v0.x helper is not recognized, only a helper of this version is.
		klog.V(4).InfoS("Backend storage classes of the claim are unknown", "claim", klog.KObj(claim), "err", err)
	}

	helper, _, err := p.helperOf(claim, classes)
	if errors.Is(err, errNotHelper) {
		return nil, nil
	}

	return helper, err
}

// backendClassesOf returns the backend StorageClasses of the hybrid StorageClass of the claim.
func (p *HybridProvisioner) backendClassesOf(claim *corev1.PersistentVolumeClaim) ([]string, error) {
	hsc, err := p.hybridClassOf(claim)
	if err != nil {
		return nil, err
	}

	return BackendClasses(hsc)
}

// hybridClassOf returns the hybrid StorageClass of the claim.
func (p *HybridProvisioner) hybridClassOf(claim *corev1.PersistentVolumeClaim) (*storagev1.StorageClass, error) {
	if claim.Spec.StorageClassName == nil {
		return nil, fmt.Errorf("persistentvolumeclaim %s has no storage class", klog.KObj(claim))
	}

	hsc, err := p.scLister.Get(*claim.Spec.StorageClassName)
	if err != nil {
		return nil, fmt.Errorf("failed to get storage class %q: %v", *claim.Spec.StorageClassName, err)
	}

	if hsc.Provisioner != DriverName {
		return nil, fmt.Errorf("storage class %q is not a hybrid storage class", hsc.Name)
	}

	return hsc, nil
}

// ownerOf returns the claim of the helper, or nil if it is gone.
func (p *HybridProvisioner) ownerOf(helper *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	owner := metav1.GetControllerOfNoCopy(helper)
	if owner == nil || owner.Kind != KindPersistentVolumeClaim {
		return nil, nil
	}

	claim, err := p.claimLister.PersistentVolumeClaims(helper.Namespace).Get(owner.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	if !isHelperOf(helper, claim) {
		return nil, nil
	}

	return claim, nil
}

// isBound reports whether the claim is bound to a PV that is moved to it.
func (p *HybridProvisioner) isBound(claim *corev1.PersistentVolumeClaim) bool {
	if claim.Spec.VolumeName == "" {
		return false
	}

	pv, err := p.volumeLister.Get(claim.Spec.VolumeName)
	if err != nil {
		return false
	}

	return boundTo(pv, claim)
}
