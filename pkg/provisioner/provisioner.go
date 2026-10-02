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
	"sync"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
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

	driverLister    storagelistersv1.CSIDriverLister
	scLister        storagelistersv1.StorageClassLister
	csiNodeLister   storagelistersv1.CSINodeLister
	nodeLister      corelisters.NodeLister
	claimLister     corelisters.PersistentVolumeClaimLister
	volumeLister    corelisters.PersistentVolumeLister
	namespaceLister corelisters.NamespaceLister

	helperTimeout  time.Duration
	maxReschedules int

	recorder record.EventRecorder
	metrics  *metrics
	clock    clock.PassiveClock

	// mirrored is the set of event keys of the helper (by UID) mirrored onto the claim, see mirrorEvents.
	mirrored sync.Map

	// faultHook is called at the fault injection points, used by e2e tests.
	faultHook func(point string)
}

// DefaultHelperTimeout is the default of Options.HelperTimeout.
const DefaultHelperTimeout = 10 * time.Minute

// Options configures the provisioner.
type Options struct {
	// HelperTimeout is how long the helper may stay pending before the claim is rescheduled, DefaultHelperTimeout if zero.
	HelperTimeout time.Duration
	// HelperMaxReschedules is how many times the claim may be rescheduled because of the helper timeout,
	// after that only events are emitted.
	HelperMaxReschedules int
	// Recorder records the events on the claim, events are dropped if it is nil.
	Recorder record.EventRecorder
}

// NewProvisioner creates a new hybrid provisioner
func NewProvisioner(
	_ context.Context,
	client kubernetes.Interface,
	listers Listers,
	opts Options,
) *HybridProvisioner {
	if opts.HelperTimeout <= 0 {
		opts.HelperTimeout = DefaultHelperTimeout
	}

	if opts.Recorder == nil {
		opts.Recorder = &record.FakeRecorder{}
	}

	return &HybridProvisioner{
		client: client,

		helperTimeout:  opts.HelperTimeout,
		maxReschedules: max(0, opts.HelperMaxReschedules),

		recorder: opts.Recorder,
		metrics:  newMetrics(),
		clock:    clock.RealClock{},

		driverLister:    listers.CSIDrivers,
		scLister:        listers.StorageClasses,
		csiNodeLister:   listers.CSINodes,
		nodeLister:      listers.Nodes,
		claimLister:     listers.Claims,
		volumeLister:    listers.Volumes,
		namespaceLister: listers.Namespaces,
	}
}

// SetFaultHook sets the function called at the fault injection points (Fault* constants).
// It is used by e2e tests to crash the controller between the steps of provisioning.
func (p *HybridProvisioner) SetFaultHook(hook func(point string)) {
	p.faultHook = hook
}

// Provision is a step function.
//
// It never waits for the backend: every call looks at the current state of the claim, the helper
// and the volume, moves it forward as far as it can, and returns ProvisioningInBackground while the backend
// is still provisioning. The library calls it again with backoff. Every step is idempotent,
// so a pass that fails at any point is continued by the next one.
func (p *HybridProvisioner) Provision(ctx context.Context, opts controller.ProvisionOptions) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	klog.V(4).InfoS("Provision: called", "PV", opts.PVName, "node", klog.KObj(opts.SelectedNode), "storageClass", klog.KObj(opts.StorageClass))

	if opts.StorageClass == nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("storageClass is required")
	}

	classes, err := BackendClasses(opts.StorageClass)
	if err != nil {
		return nil, controller.ProvisioningFinished, err
	}

	if opts.SelectedNode == nil {
		return nil, controller.ProvisioningFinished, fmt.Errorf("selected node is required, use WaitForFirstConsumer volume binding mode")
	}

	claim := opts.PVC
	node := opts.SelectedNode

	if claim.DeletionTimestamp != nil {
		// The lifecycle controller cleans up the claim that is being deleted.
		return nil, controller.ProvisioningFinished, &controller.IgnoredError{Reason: "persistentvolumeclaim is being deleted"}
	}

	// The library keeps calling Provision with the copy of the claim it stored on the last
	// ProvisioningInBackground, which has no deletionTimestamp, also after the claim is deleted.
	gone, err := p.claimGone(ctx, claim)
	if err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if gone {
		return nil, controller.ProvisioningFinished, &controller.IgnoredError{Reason: "persistentvolumeclaim is deleted or being deleted"}
	}

	helper, err := p.getHelper(claim)
	if err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if helper == nil {
		return p.provisionHelper(ctx, claim, node, classes)
	}

	legacy := !IsHelperOf(helper, claim)
	if legacy && !IsLegacyHelperOf(helper, claim, classes) {
		return nil, controller.ProvisioningFinished, fmt.Errorf("persistentvolumeclaim %s already exists and is not a helper of this claim", klog.KObj(helper))
	}

	// A deleted helper is handled by the lifecycle controller, it cannot be adopted:
	// no finalizer can be added to an object that is being deleted.
	if helper.DeletionTimestamp != nil {
		return nil, controller.ProvisioningInBackground, fmt.Errorf("waiting for helper persistentvolumeclaim %s to be deleted", klog.KObj(helper))
	}

	if claim, err = p.prepareClaim(ctx, claim, *helper.Spec.StorageClassName); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if legacy {
		if helper, err = p.adoptHelper(ctx, helper, claim); err != nil {
			return nil, controller.ProvisioningInBackground, err
		}
	}

	if helper.Spec.VolumeName == "" {
		// An empty selected node is removed by the backend, see waitHelper.
		if selected := helper.Annotations[annotationSelectedNode]; selected != "" && selected != node.Name {
			if err = p.DeleteHelper(ctx, helper, nil); err != nil {
				return nil, controller.ProvisioningInBackground, err
			}

			return nil, controller.ProvisioningInBackground, fmt.Errorf("selected node changed to %q, helper persistentvolumeclaim %s is recreated", node.Name, klog.KObj(helper))
		}

		return p.waitHelper(ctx, claim, helper, node)
	}

	pv, err := p.client.CoreV1().PersistentVolumes().Get(ctx, helper.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, controller.ProvisioningInBackground, fmt.Errorf("failed to get persistentvolume: %v", err)
		}

		// The volume is deleted (by an admin or the backend), the helper can never be bound again: a new one is created.
		if err = p.DeleteHelper(ctx, helper, nil); err != nil {
			return nil, controller.ProvisioningInBackground, err
		}

		return nil, controller.ProvisioningInBackground, fmt.Errorf("persistentvolume %s of helper persistentvolumeclaim %s is deleted, the helper is recreated", helper.Spec.VolumeName, klog.KObj(helper))
	}

	// The volume must fit the selected node, unless it has already been moved to the claim.
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID {
		if err = volume.CheckNodeAffinity(pv, node.Labels); err != nil {
			return p.discardVolume(ctx, claim, helper, pv, node, err)
		}
	}

	return p.transfer(ctx, opts, claim, helper, pv)
}

// Delete removes the storage asset that was created by Provision backing the
// given PV. Does not delete the PV object itself.
func (p *HybridProvisioner) Delete(_ context.Context, pv *corev1.PersistentVolume) (err error) {
	klog.V(4).InfoS("Delete: called", "pv", pv.Name)

	return nil
}

// VolumeFitsClaim checks that the volume can be used on the node selected for the claim.
func (p *HybridProvisioner) VolumeFitsClaim(claim *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	name := claim.Annotations[annotationSelectedNode]
	if name == "" {
		return fmt.Errorf("persistentvolumeclaim %s has no selected node", klog.KObj(claim))
	}

	node, err := p.nodeLister.Get(name)
	if err != nil {
		return fmt.Errorf("failed to get node %q: %v", name, err)
	}

	return volume.CheckNodeAffinity(pv, node.Labels)
}

// claimGone reports whether the claim is deleted, being deleted, or replaced by a claim with the same name.
// The lister may lag behind a claim that has just been created, so a miss is confirmed by the API server.
func (p *HybridProvisioner) claimGone(ctx context.Context, claim *corev1.PersistentVolumeClaim) (bool, error) {
	current, err := p.claimLister.PersistentVolumeClaims(claim.Namespace).Get(claim.Name)
	if err == nil && current.UID == claim.UID {
		return current.DeletionTimestamp != nil, nil
	}

	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("failed to get persistentvolumeclaim: %v", err)
	}

	current, err = p.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("failed to get persistentvolumeclaim: %v", err)
	}

	return current.UID != claim.UID || current.DeletionTimestamp != nil, nil
}

// provisionHelper chooses the backend StorageClass, prepares the claim and creates the helper.
func (p *HybridProvisioner) provisionHelper(
	ctx context.Context,
	claim *corev1.PersistentVolumeClaim,
	node *corev1.Node,
	classes []string,
) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	backend, err := p.chooseBackend(claim, node, classes)
	p.metrics.phase(phaseBackend, err)

	if err != nil {
		p.metrics.phases.WithLabelValues(phaseReschedule, rescheduleNoBackend).Inc()
		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonRescheduled, "No backend storage class for node %s", node.Name)

		return nil, controller.ProvisioningReschedule, err
	}

	if claim, err = p.prepareClaim(ctx, claim, backend.Name); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	helper := buildHelperPVC(claim, backend, node.Name)

	err = p.createHelper(ctx, helper)
	p.metrics.phase(phaseHelper, err)

	if err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	p.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonBackendSelected,
		"Backend storage class %s is selected for node %s, helper %s is created", backend.Name, node.Name, helper.Name)
	klog.V(2).InfoS("Helper persistentvolumeclaim created", "claim", klog.KObj(claim), "PVC", klog.KObj(helper), "storageClass", backend.Name, "node", node.Name)

	return nil, controller.ProvisioningInBackground, fmt.Errorf("waiting for storage class %q to provision helper persistentvolumeclaim %s", backend.Name, klog.KObj(helper))
}

// discardVolume deletes the helper with the volume that does not fit the selected node.
// The claim is rescheduled up to maxReschedules times, counted with the helper timeouts:
// a backend that ignores the selected node would otherwise create and delete volumes forever.
func (p *HybridProvisioner) discardVolume(
	ctx context.Context,
	claim, helper *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	node *corev1.Node,
	reason error,
) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	// The helper was provisioned for another node, a new one is created for the current node.
	if helper.Annotations[annotationSelectedNode] != node.Name {
		klog.V(2).InfoS("Persistent volume of the previous node does not fit the selected node, deleting it", "claim", klog.KObj(claim), "PV", klog.KObj(pv), "node", node.Name, "reason", reason)

		if err := p.DeleteHelper(ctx, helper, pv); err != nil {
			return nil, controller.ProvisioningInBackground, err
		}

		return nil, controller.ProvisioningInBackground, fmt.Errorf("selected node changed to %q, helper persistentvolumeclaim %s is recreated", node.Name, klog.KObj(helper))
	}

	reschedules := reschedulesOf(claim)
	if reschedules >= p.maxReschedules {
		p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonVolumeNodeMismatch,
			"Volume %s of storage class %s does not fit node %s, the claim was rescheduled %d times; delete helper %s to try again",
			pv.Name, *helper.Spec.StorageClassName, node.Name, reschedules, helper.Name)

		return nil, controller.ProvisioningInBackground, fmt.Errorf("persistentvolume %s of storage class %q does not fit node %q: %v", pv.Name, *helper.Spec.StorageClassName, node.Name, reason)
	}

	klog.V(2).InfoS("Persistent volume does not fit the selected node, deleting it", "claim", klog.KObj(claim), "PV", klog.KObj(pv), "node", node.Name, "reason", reason)

	// The claim first: a crash before the helper is deleted repeats the reschedule, but never exceeds the limit.
	if err := p.rescheduleClaim(ctx, claim); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if err := p.DeleteHelper(ctx, helper, pv); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	p.metrics.phases.WithLabelValues(phaseReschedule, rescheduleNodeAffinity).Inc()
	p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonRescheduled,
		"Volume %s of storage class %s does not fit node %s, it is deleted, rescheduling (%d of %d)",
		pv.Name, *helper.Spec.StorageClassName, node.Name, reschedules+1, p.maxReschedules)

	return nil, controller.ProvisioningReschedule, fmt.Errorf("persistentvolume %s of storage class %q does not fit node %q: %v", pv.Name, *helper.Spec.StorageClassName, node.Name, reason)
}

// waitHelper waits for the backend to provision the pending helper.
// After the helper timeout, or when the backend rejects the node, the claim is rescheduled
// up to maxReschedules times, then only events are emitted.
func (p *HybridProvisioner) waitHelper(
	ctx context.Context,
	claim, helper *corev1.PersistentVolumeClaim,
	node *corev1.Node,
) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	p.mirrorEvents(ctx, claim, helper)

	// The backend removes the selected node when it cannot provision the volume there (e.g. out of
	// capacity) and waits for the scheduler, which never selects a node for the helper again.
	rejected := helper.Annotations[annotationSelectedNode] == ""

	age := p.helperAge(helper)
	if !rejected && age < p.helperTimeout {
		return nil, controller.ProvisioningInBackground, fmt.Errorf("waiting for storage class %q to provision helper persistentvolumeclaim %s", *helper.Spec.StorageClassName, klog.KObj(helper))
	}

	cause, event, metric := fmt.Sprintf("is not provisioned in %s", age.Round(time.Second)), ReasonHelperTimeout, rescheduleTimeout
	if rejected {
		cause, event, metric = "is rejected by the backend on node "+node.Name, ReasonBackendRejected, rescheduleRejected
	}

	reschedules := reschedulesOf(claim)
	if reschedules >= p.maxReschedules {
		p.recorder.Eventf(claim, corev1.EventTypeWarning, event,
			"Helper %s of storage class %s %s, the claim was rescheduled %d times; delete helper %s to try again",
			helper.Name, *helper.Spec.StorageClassName, cause, reschedules, helper.Name)

		return nil, controller.ProvisioningInBackground, fmt.Errorf("helper persistentvolumeclaim %s %s", klog.KObj(helper), cause)
	}

	// The claim first: a crash before the helper is deleted repeats the reschedule, but never exceeds the limit.
	if err := p.rescheduleClaim(ctx, claim); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	if err := p.DeleteHelper(ctx, helper, nil); err != nil {
		return nil, controller.ProvisioningInBackground, err
	}

	p.metrics.phases.WithLabelValues(phaseReschedule, metric).Inc()
	p.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonRescheduled,
		"Helper %s of storage class %s %s, rescheduling (%d of %d)",
		helper.Name, *helper.Spec.StorageClassName, cause, reschedules+1, p.maxReschedules)

	return nil, controller.ProvisioningReschedule, fmt.Errorf("helper persistentvolumeclaim %s %s", klog.KObj(helper), cause)
}

// helperAge returns how long the helper exists.
func (p *HybridProvisioner) helperAge(helper *corev1.PersistentVolumeClaim) time.Duration {
	if helper.CreationTimestamp.IsZero() {
		return 0
	}

	return p.clock.Since(helper.CreationTimestamp.Time)
}

// reschedulesOf returns how many times the claim was rescheduled because of the helper timeout.
func reschedulesOf(claim *corev1.PersistentVolumeClaim) int {
	n, err := strconv.Atoi(claim.Annotations[AnnotationReschedules])
	if err != nil {
		return 0
	}

	return n
}

// transfer moves the volume from the helper to the claim and cleans up.
func (p *HybridProvisioner) transfer(
	ctx context.Context,
	opts controller.ProvisionOptions,
	claim, helper *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) (*corev1.PersistentVolume, controller.ProvisioningState, error) {
	pv, claim, err := p.MoveVolume(ctx, opts.StorageClass, claim, helper, pv)
	if err != nil {
		if errors.Is(err, ErrForeignVolume) {
			return nil, controller.ProvisioningFinished, err
		}

		return nil, controller.ProvisioningInBackground, err
	}

	// The claim is bound, the library does not call Provision for it again.
	// Leftovers of a failed cleanup are removed by the lifecycle controller.
	if err = p.Cleanup(ctx, claim, helper, pv); err != nil {
		klog.ErrorS(err, "Failed to clean up after provisioning", "claim", klog.KObj(claim), "PVC", klog.KObj(helper))
	}

	klog.V(2).InfoS("Provision: persistent volume provisioned", "claim", klog.KObj(claim), "PV", klog.KObj(pv), "storageClass", pv.Spec.StorageClassName)

	pv = pv.DeepCopy()
	pv.ResourceVersion = ""

	return pv, controller.ProvisioningFinished, nil
}

func (p *HybridProvisioner) fault(point string) {
	if p.faultHook != nil {
		p.faultHook(point)
	}
}
