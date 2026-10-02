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

package lifecycle

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
)

// Migration from v0.x. It is read-mostly and never changes data paths.

// Event reasons of the migration.
const (
	// ReasonReclaimPolicyMismatch: the reclaim policy of a volume differs from the hybrid StorageClass.
	ReasonReclaimPolicyMismatch = "ReclaimPolicyMismatch"
	// ReasonReclaimPolicyFixed: the reclaim policy of a volume is set to the one of the hybrid StorageClass.
	ReasonReclaimPolicyFixed = "ReclaimPolicyFixed"
	// ReasonOrphanedVolume: a PV looks like a backend volume leaked by v0.x.
	ReasonOrphanedVolume = "OrphanedVolume"
	// ReasonOrphanedHelper: a PVC looks like a helper of v0.x whose claim is gone.
	ReasonOrphanedHelper = "OrphanedHelper"
)

const (
	// legacyPodPrefix is the name prefix of the helper pods of the v0.x pod method.
	legacyPodPrefix = "provisioner-"
)

// helperNameRegexp matches the names of helper PVCs: pvc-<uid of the user PVC>.
var helperNameRegexp = regexp.MustCompile(`^pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type migrationMetrics struct {
	orphaned        prometheus.Gauge
	orphanedHelpers prometheus.Gauge
	mismatch        prometheus.Gauge
}

func newMigrationMetrics() *migrationMetrics {
	return &migrationMetrics{
		orphaned: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hybrid_orphaned_pv_total",
			Help: "Number of backend persistent volumes that look leaked by v0.x, found by the last migration pass.",
		}),
		orphanedHelpers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hybrid_orphaned_helper_pvc_total",
			Help: "Number of helper persistent volume claims of v0.x whose claim is gone, found by the last migration pass.",
		}),
		mismatch: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hybrid_reclaim_policy_mismatch_pv_total",
			Help: "Number of hybrid persistent volumes with a reclaim policy different from their hybrid storage class, found by the last migration pass.",
		}),
	}
}

// migrate is the migration pass, it runs at start and on every resync.
func (c *Controller) migrate(ctx context.Context) {
	klog.V(4).InfoS("Migration pass started")

	hybrid, backends, err := c.hybridClasses()
	if err != nil {
		klog.ErrorS(err, "Migration pass failed")

		return
	}

	c.adoptVolumes(ctx, hybrid)
	c.reportOrphans(backends)
	c.reportOrphanedHelpers(backends)
	c.deleteLegacyPods(ctx)
}

// hybridClasses returns the hybrid StorageClasses by name and the backend StorageClasses of all of them.
func (c *Controller) hybridClasses() (map[string]*storagev1.StorageClass, map[string]*storagev1.StorageClass, error) {
	classes, err := c.classes.List(labels.Everything())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list storage classes: %v", err)
	}

	byName := make(map[string]*storagev1.StorageClass, len(classes))
	for _, sc := range classes {
		byName[sc.Name] = sc
	}

	hybrid := map[string]*storagev1.StorageClass{}
	backends := map[string]*storagev1.StorageClass{}

	for _, sc := range classes {
		if sc.Provisioner != provisioner.DriverName {
			continue
		}

		hybrid[sc.Name] = sc

		names, err := provisioner.BackendClasses(sc)
		if err != nil {
			continue
		}

		for _, name := range names {
			if backend, ok := byName[name]; ok {
				backends[name] = backend
			}
		}
	}

	return hybrid, backends, nil
}

// adoptVolumes adds the hybrid metadata to the v0.x volume of every bound claim of a hybrid StorageClass,
// and reports (or fixes, with --fix-reclaim-policy) a reclaim policy that differs from the class.
func (c *Controller) adoptVolumes(ctx context.Context, hybrid map[string]*storagev1.StorageClass) {
	claims, err := c.claims.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "Failed to list persistentvolumeclaims")

		return
	}

	mismatch := 0

	for _, claim := range claims {
		if claim.Spec.StorageClassName == nil || claim.Spec.VolumeName == "" {
			continue
		}

		hsc, ok := hybrid[*claim.Spec.StorageClassName]
		if !ok {
			continue
		}

		pv, err := c.volumes.Get(claim.Spec.VolumeName)
		if err != nil || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID {
			continue
		}

		// A volume not provisioned by a backend of the class (e.g. a static PV) was not moved by v0.x.
		if pv.Labels[provisioner.LabelManaged] != provisioner.ValueTrue && !c.isBackendVolume(pv, hsc) {
			continue
		}

		differs, err := c.adoptVolume(ctx, claim, pv, hsc)
		if err != nil {
			klog.ErrorS(err, "Failed to adopt persistentvolume", "claim", klog.KObj(claim), "PV", klog.KObj(pv))
		}

		if differs {
			mismatch++
		}
	}

	c.migrationMetrics.mismatch.Set(float64(mismatch))
}

// isBackendVolume reports whether the volume was dynamically provisioned by a backend of the hybrid StorageClass.
func (c *Controller) isBackendVolume(pv *corev1.PersistentVolume, hsc *storagev1.StorageClass) bool {
	classes, err := provisioner.BackendClasses(hsc)
	if err != nil || !slices.Contains(classes, pv.Spec.StorageClassName) {
		return false
	}

	backend, err := c.classes.Get(pv.Spec.StorageClassName)
	if err != nil {
		return false
	}

	return pv.Annotations[volume.AnnDynamicallyProvisioned] == backend.Provisioner
}

// adoptVolume adopts a v0.x volume and checks its reclaim policy. It returns true if the policy differs.
//
// Only adopted v0.x volumes are checked, and only until the policy matches or is fixed once:
// the reclaim policy of a volume may be changed on purpose (e.g. to Retain to keep the data),
// it is never touched afterwards.
func (c *Controller) adoptVolume(ctx context.Context, claim *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, hsc *storagev1.StorageClass) (bool, error) {
	managed := pv.Labels[provisioner.LabelManaged] == provisioner.ValueTrue
	migrated := pv.Annotations[provisioner.AnnotationMigrated] == provisioner.ValueTrue
	checked := pv.Annotations[provisioner.AnnotationReclaimPolicyChecked] == provisioner.ValueTrue

	if managed && (!migrated || checked) {
		return false, nil
	}

	want := provisioner.ReclaimPolicy(hsc)
	policy := pv.Spec.PersistentVolumeReclaimPolicy
	mismatch := policy != want
	fix := mismatch && c.fixReclaimPolicy

	if mismatch && !fix {
		c.recorder.Eventf(claim, corev1.EventTypeWarning, ReasonReclaimPolicyMismatch,
			"Volume %s has reclaim policy %s, storage class %s has %s; the controller fixes it with --fix-reclaim-policy",
			pv.Name, policy, hsc.Name, want)
		klog.InfoS("Reclaim policy mismatch", "claim", klog.KObj(claim), "PV", klog.KObj(pv), "policy", policy, "storageClassPolicy", want)

		// Adopted already, the mismatch is reported on every pass until it is fixed.
		if managed {
			return true, nil
		}
	}

	pv = pv.DeepCopy()

	if pv.Labels == nil {
		pv.Labels = map[string]string{}
	}

	pv.Labels[provisioner.LabelManaged] = provisioner.ValueTrue

	if pv.Annotations == nil {
		pv.Annotations = map[string]string{}
	}

	pv.Annotations[provisioner.AnnotationClaim] = claim.Namespace + "/" + claim.Name
	pv.Annotations[provisioner.AnnotationStorageClass] = hsc.Name
	pv.Annotations[provisioner.AnnotationMigrated] = provisioner.ValueTrue

	if !mismatch || fix {
		pv.Annotations[provisioner.AnnotationReclaimPolicyChecked] = provisioner.ValueTrue
	}

	if fix {
		pv.Spec.PersistentVolumeReclaimPolicy = want
	}

	if _, err := c.client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{}); err != nil {
		return mismatch, fmt.Errorf("failed to update persistentvolume: %v", err)
	}

	if fix {
		c.recorder.Eventf(claim, corev1.EventTypeNormal, ReasonReclaimPolicyFixed,
			"Reclaim policy of volume %s is changed from %s to %s of storage class %s", pv.Name, policy, want, hsc.Name)
	}

	if !managed {
		klog.V(2).InfoS("Persistent volume of a previous version adopted", "claim", klog.KObj(claim), "PV", klog.KObj(pv))
	}

	return mismatch && !fix, nil
}

// reportOrphans reports backend volumes leaked by v0.x. They are never deleted:
// we cannot prove that we own them.
//
// A leaked volume is a dynamically provisioned PV of a backend of some hybrid StorageClass
// with the Retain policy (v0.x switched it to Retain to move it), and either no claim
// (v0.x crashed after claimRef = null) or Released from a claim named like a helper PVC.
func (c *Controller) reportOrphans(backends map[string]*storagev1.StorageClass) {
	pvs, err := c.volumes.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "Failed to list persistentvolumes")

		return
	}

	orphaned := 0

	for _, pv := range pvs {
		if !isOrphan(pv, backends) {
			continue
		}

		orphaned++

		c.recorder.Eventf(pv, corev1.EventTypeWarning, ReasonOrphanedVolume,
			"Volume of storage class %s looks leaked by a previous version of the hybrid plugin, check and delete it manually",
			pv.Spec.StorageClassName)
		klog.InfoS("Orphaned persistent volume found", "PV", klog.KObj(pv), "storageClass", pv.Spec.StorageClassName, "claimRef", pv.Spec.ClaimRef)
	}

	c.migrationMetrics.orphaned.Set(float64(orphaned))
}

func isOrphan(pv *corev1.PersistentVolume, backends map[string]*storagev1.StorageClass) bool {
	if pv.Labels[provisioner.LabelManaged] == provisioner.ValueTrue || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return false
	}

	backend, ok := backends[pv.Spec.StorageClassName]
	if !ok || pv.Annotations[volume.AnnDynamicallyProvisioned] != backend.Provisioner {
		return false
	}

	if pv.Spec.ClaimRef == nil {
		return true
	}

	return pv.Status.Phase == corev1.VolumeReleased && helperNameRegexp.MatchString(pv.Spec.ClaimRef.Name)
}

// reportOrphanedHelpers reports helper PVCs of v0.x whose claim is gone: the claim was deleted
// before this version adopted the helper. They are never deleted: without the claim we cannot prove that we own them.
//
// Such a PVC is named like a helper, pvc-<uid>, uses a backend StorageClass of some hybrid
// StorageClass, is not adopted (no owner-uid), and no PVC in its namespace has that uid.
func (c *Controller) reportOrphanedHelpers(backends map[string]*storagev1.StorageClass) {
	claims, err := c.claims.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "Failed to list persistentvolumeclaims")

		return
	}

	uids := make(map[types.UID]bool, len(claims))
	for _, claim := range claims {
		uids[claim.UID] = true
	}

	orphaned := 0

	for _, pvc := range claims {
		if !helperNameRegexp.MatchString(pvc.Name) || pvc.Spec.StorageClassName == nil {
			continue
		}

		if _, ok := pvc.Annotations[provisioner.AnnotationOwnerUID]; ok {
			continue
		}

		if _, ok := backends[*pvc.Spec.StorageClassName]; !ok {
			continue
		}

		if uids[types.UID(strings.TrimPrefix(pvc.Name, "pvc-"))] {
			continue
		}

		orphaned++

		c.recorder.Eventf(pvc, corev1.EventTypeWarning, ReasonOrphanedHelper,
			"Claim looks like a helper of a previous version of the hybrid plugin whose claim is gone, check and delete it manually")
		klog.InfoS("Orphaned helper persistent volume claim found", "PVC", klog.KObj(pvc), "storageClass", *pvc.Spec.StorageClassName)
	}

	c.migrationMetrics.orphanedHelpers.Set(float64(orphaned))
}

// deleteLegacyPods deletes the helper pods of the v0.x pod method once their helper PVC is bound or gone.
// This version never creates them, so it stops looking after a pass with nothing left to delete.
func (c *Controller) deleteLegacyPods(ctx context.Context) {
	if c.legacyPodsDone {
		return
	}

	// From the watch cache of the API server, this pass does not need the newest state.
	pods, err := c.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		klog.ErrorS(err, "Failed to list pods")

		return
	}

	left := 0

	for i := range pods.Items {
		pod := &pods.Items[i]

		helperName, ok := legacyPodHelper(pod)
		if !ok {
			continue
		}

		helper, err := c.claims.PersistentVolumeClaims(pod.Namespace).Get(helperName)
		if err != nil && !apierrors.IsNotFound(err) {
			left++

			continue
		}

		if helper != nil && helper.Spec.VolumeName == "" && helper.DeletionTimestamp == nil {
			// The backend is still provisioning, the pod may be needed (v0.x pod method).
			left++

			continue
		}

		err = c.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pod.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete helper pod of a previous version", "pod", klog.KObj(pod))

			left++

			continue
		}

		klog.V(2).InfoS("Helper pod of a previous version deleted", "pod", klog.KObj(pod), "helper", helperName)
	}

	c.legacyPodsDone = left == 0
}

// legacyPodHelper returns the helper PVC name of a v0.x helper pod: provisioner-<helper>
// with the helper PVC as its only PVC volume. Other volumes are allowed: v0.x did not
// disable the service account token, admission adds a projected volume for it.
func legacyPodHelper(pod *corev1.Pod) (string, bool) {
	helperName, ok := strings.CutPrefix(pod.Name, legacyPodPrefix)
	if !ok || !helperNameRegexp.MatchString(helperName) {
		return "", false
	}

	claims := 0

	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}

		if v.PersistentVolumeClaim.ClaimName != helperName {
			return "", false
		}

		claims++
	}

	return helperName, claims == 1
}
