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
// It queues the user PVCs and helper PVCs that the provisioning library does not handle,
// and passes them to provisioner.HybridProvisioner.Reconcile: deletion of the user PVC while
// provisioning is in progress, deletion of the helper PVC by someone else and the cleanup left
// over after a crash. It also runs the migration of volumes from v0.x.
package lifecycle

import (
	"context"
	"fmt"
	"slices"
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

// ResyncPeriod is how often all watched objects are reconciled again.
const ResyncPeriod = 10 * time.Minute

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

	after, err := c.prov.Reconcile(ctx, pvc)
	if err != nil {
		return err
	}

	if after > 0 {
		c.queue.AddAfter(key, after)
	}

	return nil
}
