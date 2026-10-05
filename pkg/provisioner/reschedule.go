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
	"strconv"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// rescheduleCause is why the claim cannot get a volume on the selected node.
type rescheduleCause struct {
	// result is the result of the reschedule phase in hybrid_provision_phase_total.
	result string
	// counted is set when a helper was created on the node: the reschedule deletes it,
	// is counted and is limited by maxReschedules.
	counted bool
	// giveUp is the event reason of a counted cause when the claim is not rescheduled any more.
	giveUp string
	// message says what happened, it is used in the events and the error.
	message string
}

func (c rescheduleCause) err() error {
	return errors.New(c.message)
}

// causeNoBackend: no backend StorageClass can serve the node, nothing is created on it.
func causeNoBackend(node *corev1.Node, reason error) rescheduleCause {
	return rescheduleCause{
		result:  rescheduleNoBackend,
		message: fmt.Sprintf("no backend storage class for node %s: %v", node.Name, reason),
	}
}

// causeHelperTimeout: the helper is still pending after the helper timeout.
func causeHelperTimeout(helper *corev1.PersistentVolumeClaim, age time.Duration) rescheduleCause {
	return rescheduleCause{
		result:  rescheduleTimeout,
		counted: true,
		giveUp:  ReasonHelperTimeout,
		message: fmt.Sprintf("helper %s of storage class %s is not provisioned in %s", helper.Name, *helper.Spec.StorageClassName, age.Round(time.Second)),
	}
}

// causeBackendRejected: the backend removed the selected node from the helper.
func causeBackendRejected(helper *corev1.PersistentVolumeClaim, node *corev1.Node) rescheduleCause {
	return rescheduleCause{
		result:  rescheduleRejected,
		counted: true,
		giveUp:  ReasonBackendRejected,
		message: fmt.Sprintf("helper %s of storage class %s is rejected by the backend on node %s", helper.Name, *helper.Spec.StorageClassName, node.Name),
	}
}

// causeVolumeNodeMismatch: the backend created the volume of the helper where the node cannot use it.
func causeVolumeNodeMismatch(helper *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, node *corev1.Node, reason error) rescheduleCause {
	return rescheduleCause{
		result:  rescheduleNodeAffinity,
		counted: true,
		giveUp:  ReasonVolumeNodeMismatch,
		message: fmt.Sprintf("volume %s of storage class %s does not fit node %s: %v", pv.Name, *helper.Spec.StorageClassName, node.Name, reason),
	}
}

// reschedule asks the scheduler to choose another node for the claim.
//
// A cause that is not counted created nothing on the node: the library removes the selected node.
// For a counted cause the helper is deleted with its volume, if it has one, and the reschedule is
// counted in AnnotationReschedules: after maxReschedules the claim is not rescheduled any more,
// only the giveUp event is emitted and the claim keeps waiting, so that a pod is not moved around
// the cluster forever.
func (p *HybridProvisioner) reschedule(
	ctx context.Context,
	claim, helper *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	cause rescheduleCause,
) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	if !cause.counted {
		p.metrics.phases.WithLabelValues(phaseReschedule, cause.result).Inc()
		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonRescheduled, "Rescheduling: %s", cause.message)

		return nil, controller.ProvisioningReschedule, cause.err()
	}

	reschedules := reschedulesOf(claim)
	if reschedules >= p.maxReschedules {
		p.recorder.Eventf(claim, corev1.EventTypeWarning, cause.giveUp,
			"Not rescheduled any more, the claim was rescheduled %d times: %s; delete helper %s to try again",
			reschedules, cause.message, helper.Name)

		return nil, controller.ProvisioningInBackground, cause.err()
	}

	klog.V(2).InfoS("Rescheduling the claim", "claim", klog.KObj(claim), "PVC", klog.KObj(helper), "cause", cause.message)

	// The claim first: a crash before the helper is deleted repeats the reschedule, but never exceeds the limit.
	if err := p.rescheduleClaim(ctx, claim); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if err := p.deleteHelper(ctx, helper, pv); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	p.metrics.phases.WithLabelValues(phaseReschedule, cause.result).Inc()
	p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonRescheduled,
		"Rescheduling (%d of %d): %s", reschedules+1, p.maxReschedules, cause.message)

	return nil, controller.ProvisioningReschedule, cause.err()
}

// reschedulesOf returns how many times the claim was rescheduled, see reschedule.
func reschedulesOf(claim *corev1.PersistentVolumeClaim) int {
	n, err := strconv.Atoi(claim.Annotations[AnnotationReschedules])
	if err != nil {
		return 0
	}

	return n
}

// rescheduleClaim prepares the claim for a reschedule in one write: unpins the backend StorageClass,
// removes the selected node and counts the reschedule.
//
// The library removes the selected node itself after ProvisioningReschedule, but with the claim
// it passed to Provision(), which is stale after this write, so it is done here.
func (p *HybridProvisioner) rescheduleClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	claim = claim.DeepCopy()
	delete(claim.Annotations, AnnotationBackendClass)
	delete(claim.Annotations, annotationSelectedNode)

	if claim.Annotations == nil {
		claim.Annotations = map[string]string{}
	}

	claim.Annotations[AnnotationReschedules] = strconv.Itoa(reschedulesOf(claim) + 1)

	if _, err := p.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update persistentvolumeclaim: %v", err)
	}

	return nil
}
