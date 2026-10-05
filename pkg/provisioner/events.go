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
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog/v2"
)

// Event reasons on the user PVC.
const (
	// ReasonBackendSelected: the backend StorageClass is chosen and the helper is created.
	ReasonBackendSelected = "BackendSelected"
	// ReasonWaitingForBackend mirrors a Warning event of the helper.
	ReasonWaitingForBackend = "WaitingForBackend"
	// ReasonRescheduled: the claim is rescheduled to another node.
	ReasonRescheduled = "Rescheduled"
	// ReasonHelperTimeout: the helper is not provisioned in time and the claim is not rescheduled any more.
	ReasonHelperTimeout = "HelperTimeout"
	// ReasonBackendRejected: the backend cannot provision the helper on the node and the claim is not rescheduled any more.
	ReasonBackendRejected = "BackendRejected"
	// ReasonVolumeNodeMismatch: the volume does not fit the selected node and the claim is not rescheduled any more.
	ReasonVolumeNodeMismatch = "VolumeNodeMismatch"

	// ReasonProvisioningCanceled: the claim was deleted before it was bound, the helper is deleted.
	ReasonProvisioningCanceled = "ProvisioningCanceled"
	// ReasonHelperDeleted: the helper was deleted by someone else before the volume was provisioned.
	ReasonHelperDeleted = "HelperDeleted"
	// ReasonVolumeRecovered: the helper was deleted by someone else, its volume is moved to the claim.
	ReasonVolumeRecovered = "VolumeRecovered"
	// ReasonCleanupFinished: the leftovers of an interrupted provisioning are cleaned up.
	ReasonCleanupFinished = "CleanupFinished"
)

// maxMirroredEvents limits the number of helper events mirrored onto the claim in one pass.
const maxMirroredEvents = 5

// mirrorEvents copies the new Warning events of the helper onto the claim.
// They tell the user why the backend does not provision the volume (quota, capacity, ...).
//
// Events are identified by UID and count: event timestamps have a resolution of a second,
// and a repeated event keeps its UID with a higher count.
func (p *HybridProvisioner) mirrorEvents(ctx context.Context, claim, helper *corev1.PersistentVolumeClaim) {
	list, err := p.client.CoreV1().Events(helper.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.Set{
			"involvedObject.uid": string(helper.UID),
			"type":               corev1.EventTypeWarning,
		}.String(),
	})
	if err != nil {
		klog.V(4).InfoS("Failed to list events of helper persistentvolumeclaim", "PVC", klog.KObj(helper), "err", err)

		return
	}

	mirrored, _ := p.mirrored.Load(helper.UID)
	seen, _ := mirrored.(map[string]bool)

	// Only the keys of the current events are kept, expired events are forgotten.
	current := map[string]bool{}

	var events []corev1.Event

	for _, ev := range list.Items {
		if ev.InvolvedObject.UID != helper.UID || ev.Type != corev1.EventTypeWarning {
			continue
		}

		key := eventKey(&ev)
		current[key] = true

		if !seen[key] {
			events = append(events, ev)
		}
	}

	p.mirrored.Store(helper.UID, current)

	slices.SortFunc(events, func(a, b corev1.Event) int { return eventTime(&a).Compare(eventTime(&b)) })

	for _, ev := range events[max(0, len(events)-maxMirroredEvents):] {
		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonWaitingForBackend,
			"Helper %s: %s: %s", helper.Name, ev.Reason, ev.Message)
	}
}

// eventKey identifies an occurrence of the event.
func eventKey(ev *corev1.Event) string {
	count := ev.Count
	if ev.Series != nil {
		count = ev.Series.Count
	}

	return string(ev.UID) + "/" + strconv.Itoa(int(count))
}

// eventTime returns when the event was last observed.
func eventTime(ev *corev1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}

	return ev.CreationTimestamp.Time
}
