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

// Package lifecycle is the lifecycle controller of hybrid volumes.
//
// It handles everything the provisioning library does not: deletion of the user PVC
// while provisioning is in progress, deletion of the helper PVC by someone else,
// the cleanup left over after a crash, and the migration of volumes from v0.x.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// ResyncPeriod is how often all watched objects are reconciled again.
	ResyncPeriod = 10 * time.Minute

	// cleanupDelay gives the Provision() pass that has just bound the claim the time to finish the cleanup itself,
	// before the lifecycle controller treats them as leftovers.
	cleanupDelay = 30 * time.Second
)

// Event reasons.
const (
	// ReasonProvisioningCanceled: the claim was deleted before it was bound, the helper is deleted.
	ReasonProvisioningCanceled = "ProvisioningCanceled"
	// ReasonHelperDeleted: the helper was deleted by someone else before the volume was provisioned.
	ReasonHelperDeleted = "HelperDeleted"
	// ReasonVolumeRecovered: the helper was deleted by someone else, its volume is moved to the claim.
	ReasonVolumeRecovered = "VolumeRecovered"
	// ReasonCleanupFinished: the leftovers of an interrupted provisioning are cleaned up.
	ReasonCleanupFinished = "CleanupFinished"
)

// Options configures the lifecycle controller.
type Options struct {
	// FixReclaimPolicy makes the migration set the reclaim policy of v0.x volumes
	// to the one of their hybrid StorageClass, instead of only reporting the difference.
	FixReclaimPolicy bool
}

// Controller is the lifecycle controller.
type Controller struct {
	client   kubernetes.Interface
	prov     *provisioner.HybridProvisioner
	recorder record.EventRecorder

	fixReclaimPolicy bool
	migrationMetrics *migrationMetrics
	// legacyPodsDone is set once no v0.x helper pods are left, only the migration pass uses it.
	legacyPodsDone bool

	claims  corelisters.PersistentVolumeClaimLister
	volumes corelisters.PersistentVolumeLister
	classes storagelistersv1.StorageClassLister
	synced  []cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]

	// cleanupDelay is the grace period before the leftovers of a bound claim are cleaned up.
	cleanupDelay time.Duration
	// boundSince is when the claim (by key) was first seen bound with leftovers.
	boundSince sync.Map
}

// New creates the lifecycle controller and registers its event handlers in the informer factory.
func New(
	client kubernetes.Interface,
	prov *provisioner.HybridProvisioner,
	factory informers.SharedInformerFactory,
	recorder record.EventRecorder,
	opts Options,
) (*Controller, error) {
	claimInformer := factory.Core().V1().PersistentVolumeClaims()
	volumeInformer := factory.Core().V1().PersistentVolumes()
	classInformer := factory.Storage().V1().StorageClasses()

	c := &Controller{
		client:   client,
		prov:     prov,
		recorder: recorder,

		fixReclaimPolicy: opts.FixReclaimPolicy,
		migrationMetrics: newMigrationMetrics(),

		claims:  claimInformer.Lister(),
		volumes: volumeInformer.Lister(),
		classes: classInformer.Lister(),
		synced: []cache.InformerSynced{
			claimInformer.Informer().HasSynced,
			volumeInformer.Informer().HasSynced,
			classInformer.Informer().HasSynced,
		},

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "hybrid-lifecycle"},
		),

		cleanupDelay: cleanupDelay,
	}

	if _, err := claimInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueClaim,
		UpdateFunc: func(_, obj any) { c.enqueueClaim(obj) },
	}, ResyncPeriod); err != nil {
		return nil, fmt.Errorf("failed to add persistentvolumeclaim event handler: %v", err)
	}

	if _, err := volumeInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueueVolume,
		UpdateFunc: func(_, obj any) { c.enqueueVolume(obj) },
	}, ResyncPeriod); err != nil {
		return nil, fmt.Errorf("failed to add persistentvolume event handler: %v", err)
	}

	return c, nil
}

// Collectors returns the metrics of the controller.
func (c *Controller) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "hybrid_helper_pvcs",
			Help: "Number of helper persistent volume claims.",
		}, func() float64 {
			helpers, err := c.claims.List(labels.SelectorFromSet(labels.Set{provisioner.LabelRole: provisioner.LabelRoleHelper}))
			if err != nil {
				return 0
			}

			return float64(len(helpers))
		}),
		c.migrationMetrics.orphaned,
		c.migrationMetrics.orphanedHelpers,
		c.migrationMetrics.mismatch,
	}
}

// Run starts the workers and blocks until the context is done.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("Starting lifecycle controller")
	defer klog.InfoS("Shutting down lifecycle controller")

	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return
	}

	for range workers {
		go wait.UntilWithContext(ctx, c.worker, time.Second)
	}

	// Migration from v0.x: at start and on every resync.
	go wait.UntilWithContext(ctx, c.migrate, ResyncPeriod)

	<-ctx.Done()
}

// enqueueClaim queues a claim with the provisioning finalizer, and a helper with its owner claim.
func (c *Controller) enqueueClaim(obj any) {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return
	}

	switch {
	case pvc.Labels[provisioner.LabelRole] == provisioner.LabelRoleHelper:
		c.queue.Add(pvc.Namespace + "/" + pvc.Name)

		if owner := metav1.GetControllerOfNoCopy(pvc); owner != nil && owner.Kind == provisioner.KindPersistentVolumeClaim {
			c.queue.Add(pvc.Namespace + "/" + owner.Name)
		}
	case slices.Contains(pvc.Finalizers, provisioner.FinalizerProvisioning):
		c.queue.Add(pvc.Namespace + "/" + pvc.Name)
	}
}

// enqueueVolume queues the claim of a hybrid-managed PV.
func (c *Controller) enqueueVolume(obj any) {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok || pv.Labels[provisioner.LabelManaged] != provisioner.ValueTrue || pv.Spec.ClaimRef == nil {
		return
	}

	c.queue.Add(pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name)
}

func (c *Controller) worker(ctx context.Context) {
	for c.processNext(ctx) {
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(key)

	if err := c.sync(ctx, key); err != nil {
		klog.ErrorS(err, "Failed to reconcile persistentvolumeclaim", "key", key)
		c.queue.AddRateLimited(key)

		return true
	}

	c.queue.Forget(key)

	return true
}

// sync reconciles one PVC, a user claim or a helper.
func (c *Controller) sync(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil //nolint:nilerr // invalid keys are never retried
	}

	pvc, err := c.claims.PersistentVolumeClaims(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	if pvc.Labels[provisioner.LabelRole] == provisioner.LabelRoleHelper {
		return c.syncHelper(ctx, pvc)
	}

	return c.syncClaim(ctx, pvc)
}

// syncClaim reconciles a user claim.
func (c *Controller) syncClaim(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	switch {
	case claim.DeletionTimestamp != nil:
		if !slices.Contains(claim.Finalizers, provisioner.FinalizerProvisioning) {
			return nil
		}

		// The claim is deleted before it is bound, or after a crash before the cleanup is finished.
		helper, err := c.helperOf(claim)
		if err != nil {
			return err
		}

		if err = c.prov.Cleanup(ctx, claim, helper, nil); err != nil {
			return err
		}

		if helper != nil && claim.Spec.VolumeName == "" {
			c.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonProvisioningCanceled,
				"Claim is deleted during provisioning, helper %s is deleted", helper.Name)
		}

		klog.V(2).InfoS("Claim is released", "claim", klog.KObj(claim), "helper", klog.KObj(helper))

	case c.isBound(claim):
		return c.cleanupBound(ctx, claim)
	}

	// The claim is being provisioned, Provision() owns it.
	return nil
}

// cleanupBound removes the leftovers of an interrupted cleanup of a bound claim.
func (c *Controller) cleanupBound(ctx context.Context, claim *corev1.PersistentVolumeClaim) error {
	helper, err := c.helperOf(claim)
	if err != nil {
		return err
	}

	key := claim.Namespace + "/" + claim.Name

	if helper == nil && !slices.Contains(claim.Finalizers, provisioner.FinalizerProvisioning) {
		c.boundSince.Delete(key)

		return nil
	}

	now := time.Now()
	since, _ := c.boundSince.LoadOrStore(key, now)

	if wait := c.cleanupDelay - now.Sub(since.(time.Time)); wait > 0 {
		c.queue.AddAfter(key, wait)

		return nil
	}

	if err = c.prov.Cleanup(ctx, claim, helper, nil); err != nil {
		return err
	}

	c.boundSince.Delete(key)

	c.recorder.Event(claim, corev1.EventTypeNormal, ReasonCleanupFinished, "Provisioning cleanup is finished")
	klog.V(2).InfoS("Provisioning cleanup is finished", "claim", klog.KObj(claim), "helper", klog.KObj(helper))

	return nil
}

// syncHelper reconciles a helper.
func (c *Controller) syncHelper(ctx context.Context, helper *corev1.PersistentVolumeClaim) error {
	if helper.Annotations[provisioner.AnnotationOwnerUID] == "" {
		return nil
	}

	claim, err := c.ownerOf(helper)
	if err != nil {
		return err
	}

	if helper.DeletionTimestamp != nil {
		if !slices.Contains(helper.Finalizers, provisioner.FinalizerHelper) {
			return nil
		}

		return c.syncDeletedHelper(ctx, claim, helper)
	}

	if claim != nil && c.isBound(claim) {
		// The volume is moved, the helper is a leftover.
		return c.cleanupBound(ctx, claim)
	}

	// The claim is gone: the garbage collector deletes the helper by its owner reference, then it is handled above.
	return nil
}

// syncDeletedHelper handles a helper deleted by someone else.
func (c *Controller) syncDeletedHelper(ctx context.Context, claim, helper *corev1.PersistentVolumeClaim) error {
	switch {
	case claim == nil || claim.DeletionTimestamp != nil:
		// The claim is gone, the fresh volume is deleted with the helper.
		if err := c.prov.DeleteHelper(ctx, helper, nil); err != nil {
			return err
		}

		klog.V(2).InfoS("Helper of a deleted claim is released", "helper", klog.KObj(helper))

	case helper.Spec.VolumeName != "":
		// The helper is bound, the volume is kept: finish the move.
		return c.recoverVolume(ctx, claim, helper)

	default:
		// The helper is pending, the next Provision() pass creates a new one.
		if err := c.prov.DeleteHelper(ctx, helper, nil); err != nil {
			return err
		}

		c.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonHelperDeleted,
			"Helper %s is deleted before the volume was provisioned, it is created again", helper.Name)
		klog.V(2).InfoS("Pending helper is deleted by someone else", "claim", klog.KObj(claim), "helper", klog.KObj(helper))
	}

	return nil
}

// recoverVolume moves the volume of the deleted helper to the claim.
func (c *Controller) recoverVolume(ctx context.Context, claim, helper *corev1.PersistentVolumeClaim) error {
	pv, err := c.volumes.Get(helper.Spec.VolumeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.prov.DeleteHelper(ctx, helper, nil)
		}

		return err
	}

	// As in Provision(), a volume that does not fit the selected node is discarded, the next pass creates a new helper.
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID {
		if reason := c.prov.VolumeFitsClaim(claim, pv); reason != nil {
			if err = c.prov.DeleteHelper(ctx, helper, pv); err != nil {
				return err
			}

			c.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonHelperDeleted,
				"Helper %s is deleted by someone else, its volume %s is not used: %v; the helper is created again", helper.Name, pv.Name, reason)
			klog.V(2).InfoS("Volume of a deleted helper does not fit the claim, it is discarded", "claim", klog.KObj(claim), "helper", klog.KObj(helper), "PV", klog.KObj(pv), "reason", reason)

			return nil
		}
	}

	if claim.Spec.StorageClassName == nil {
		return fmt.Errorf("persistentvolumeclaim %s has no storage class", klog.KObj(claim))
	}

	hsc, err := c.classes.Get(*claim.Spec.StorageClassName)
	if err != nil {
		return fmt.Errorf("failed to get storage class %q: %v", *claim.Spec.StorageClassName, err)
	}

	pv, claim, err = c.prov.MoveVolume(ctx, hsc, claim, helper, pv)
	if err != nil {
		if errors.Is(err, provisioner.ErrForeignVolume) {
			// The volume is not ours, the helper is released without touching it.
			klog.InfoS("Helper volume is bound to another claim, it is not moved", "helper", klog.KObj(helper), "error", err)

			return c.prov.DeleteHelper(ctx, helper, nil)
		}

		return err
	}

	if err = c.prov.Cleanup(ctx, claim, helper, pv); err != nil {
		return err
	}

	c.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonVolumeRecovered,
		"Helper %s is deleted by someone else, volume %s is moved to the claim", helper.Name, pv.Name)
	klog.V(2).InfoS("Volume of a deleted helper is moved to the claim", "claim", klog.KObj(claim), "helper", klog.KObj(helper), "PV", klog.KObj(pv))

	return nil
}

// helperOf returns the helper of the claim, or nil if there is none. A helper created by v0.x
// and not adopted yet is returned too: the claim may be deleted before Provision() adopts it.
func (c *Controller) helperOf(claim *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	helper, err := c.claims.PersistentVolumeClaims(claim.Namespace).Get(provisioner.HelperName(claim))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	if provisioner.IsHelperOf(helper, claim) || c.isLegacyHelperOf(helper, claim) {
		return helper, nil
	}

	return nil, nil
}

// isLegacyHelperOf reports whether the helper was created by v0.x for the claim, see provisioner.IsLegacyHelperOf.
func (c *Controller) isLegacyHelperOf(helper, claim *corev1.PersistentVolumeClaim) bool {
	if claim.Spec.StorageClassName == nil {
		return false
	}

	hsc, err := c.classes.Get(*claim.Spec.StorageClassName)
	if err != nil || hsc.Provisioner != provisioner.DriverName {
		return false
	}

	classes, err := provisioner.BackendClasses(hsc)
	if err != nil {
		return false
	}

	return provisioner.IsLegacyHelperOf(helper, claim, classes)
}

// ownerOf returns the claim of the helper, or nil if it is gone.
func (c *Controller) ownerOf(helper *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	owner := metav1.GetControllerOfNoCopy(helper)
	if owner == nil || owner.Kind != provisioner.KindPersistentVolumeClaim {
		return nil, nil
	}

	claim, err := c.claims.PersistentVolumeClaims(helper.Namespace).Get(owner.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	if !provisioner.IsHelperOf(helper, claim) {
		return nil, nil
	}

	return claim, nil
}

// isBound reports whether the claim is bound to a PV that is moved to it.
func (c *Controller) isBound(claim *corev1.PersistentVolumeClaim) bool {
	if claim.Spec.VolumeName == "" {
		return false
	}

	pv, err := c.volumes.Get(claim.Spec.VolumeName)
	if err != nil {
		return false
	}

	return pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == claim.UID
}
