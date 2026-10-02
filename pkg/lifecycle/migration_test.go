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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/component-helpers/storage/volume"
)

const legacyHelperName = "pvc-0a1b2c3d-0000-4000-8000-000000000001"

// testLegacyBound is a claim bound by v0.x: no hybrid metadata on the volume.
func testLegacyBound(policy corev1.PersistentVolumeReclaimPolicy) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	claim := testClaim()
	claim.Finalizers = nil
	claim.Annotations = nil

	pv := testVolume(claim)
	pv.Spec.PersistentVolumeReclaimPolicy = policy
	claim.Spec.VolumeName = pv.Name

	return claim, pv
}

// testOrphan is a backend PV leaked by v0.x.
func testOrphan(name string, claimRef *corev1.ObjectReference, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	pv := testVolume(testClaim())
	pv.Name = name
	pv.Spec.ClaimRef = claimRef
	pv.Status.Phase = phase

	return pv
}

// testLegacyPod is a helper pod of the v0.x pod method, with the service account token
// volume that admission adds to it.
func testLegacyPod(name string, claimNames ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: "uid-pod"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "kube-api-access-abcde",
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}},
			}},
		},
	}

	for _, claimName := range claimNames {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "provisioner",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
			},
		})
	}

	return pod
}

func (e *testEnv) migrate() {
	e.t.Helper()

	e.ctrl.migrate(e.t.Context())
}

// waitVolumeSynced waits until the informer has the stored version of the PV.
func (e *testEnv) waitVolumeSynced(name string) {
	e.t.Helper()

	stored := e.getPV(name)

	err := wait.PollUntilContextTimeout(e.t.Context(), 5*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		cached, err := e.ctrl.volumes.Get(name)

		return err == nil && cached.ResourceVersion == stored.ResourceVersion, nil
	})
	if err != nil {
		e.t.Fatalf("informer did not sync PV %s: %v", name, err)
	}
}

// updatePV updates the PV as an admin would and waits for the informer.
func (e *testEnv) updatePV(pv *corev1.PersistentVolume) {
	e.t.Helper()

	if _, err := e.client.CoreV1().PersistentVolumes().Update(e.t.Context(), pv, metav1.UpdateOptions{}); err != nil {
		e.t.Fatalf("failed to update PV %s: %v", pv.Name, err)
	}

	e.waitVolumeSynced(pv.Name)
}

func TestMigrateAdoptVolume(t *testing.T) {
	claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimDelete)

	// A claim of another class is not touched.
	other := testClaim()
	other.Name = "other"
	other.UID = "uid-other"
	other.Spec.StorageClassName = new(testBackend)

	env := newTestEnv(t, append(testClasses(), claim, pv, other)...)

	env.migrate()

	got := env.getPV(pv.Name)
	if got.Labels[provisioner.LabelManaged] != provisioner.ValueTrue {
		t.Errorf("PV label %s = %q, want true", provisioner.LabelManaged, got.Labels[provisioner.LabelManaged])
	}

	for k, v := range map[string]string{
		provisioner.AnnotationClaim:        testNamespace + "/" + testClaimName,
		provisioner.AnnotationStorageClass: testHybridClass,
		provisioner.AnnotationMigrated:     provisioner.ValueTrue,
	} {
		if got.Annotations[k] != v {
			t.Errorf("PV annotation %s = %q, want %q", k, got.Annotations[k], v)
		}
	}

	if events := env.events(); len(events) != 0 {
		t.Errorf("events = %v, want none", events)
	}

	// The next pass does nothing, once the informer has seen the adoption.
	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		cached, err := env.ctrl.volumes.Get(pv.Name)

		return err == nil && cached.Annotations[provisioner.AnnotationMigrated] == provisioner.ValueTrue, nil
	})
	if err != nil {
		t.Fatalf("informer did not sync: %v", err)
	}

	writes := env.writes

	env.migrate()

	if env.writes != writes {
		t.Errorf("second pass made %d writes, want 0", env.writes-writes)
	}
}

func TestMigrateReclaimPolicy(t *testing.T) {
	t.Run("mismatch is reported", func(t *testing.T) {
		claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
		env := newTestEnv(t, append(testClasses(), claim, pv)...)

		for range 2 {
			env.migrate()

			if got := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimRetain {
				t.Errorf("PV reclaim policy = %s, want Retain (not fixed)", got)
			}

			env.requireEvent(ReasonReclaimPolicyMismatch)

			if got := testutil.ToFloat64(env.ctrl.migrationMetrics.mismatch); got != 1 {
				t.Errorf("hybrid_reclaim_policy_mismatch_pv_total = %v, want 1", got)
			}
		}
	})

	t.Run("mismatch is fixed", func(t *testing.T) {
		claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
		env := newTestEnv(t, append(testClasses(), claim, pv)...)
		env.ctrl.fixReclaimPolicy = true

		env.migrate()

		if got := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimDelete {
			t.Errorf("PV reclaim policy = %s, want Delete", got)
		}

		env.requireEvent(ReasonReclaimPolicyFixed)

		if got := testutil.ToFloat64(env.ctrl.migrationMetrics.mismatch); got != 0 {
			t.Errorf("hybrid_reclaim_policy_mismatch_pv_total = %v, want 0", got)
		}
	})

	// A mismatch reported while the flag was off is fixed once it is turned on.
	t.Run("mismatch is fixed later", func(t *testing.T) {
		claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
		env := newTestEnv(t, append(testClasses(), claim, pv)...)

		env.migrate()
		env.requireEvent(ReasonReclaimPolicyMismatch)
		env.waitVolumeSynced(pv.Name)

		env.ctrl.fixReclaimPolicy = true
		env.migrate()

		if got := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimDelete {
			t.Errorf("PV reclaim policy = %s, want Delete", got)
		}

		env.requireEvent(ReasonReclaimPolicyFixed)
	})

	// After the fix the policy is changed back to Retain by an admin: it is kept.
	t.Run("fixed volume is not touched again", func(t *testing.T) {
		claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
		env := newTestEnv(t, append(testClasses(), claim, pv)...)
		env.ctrl.fixReclaimPolicy = true

		env.migrate()
		env.events()
		env.waitVolumeSynced(pv.Name)

		fixed := env.getPV(pv.Name)
		fixed.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		env.updatePV(fixed)

		writes := env.writes

		env.migrate()

		if got := env.getPV(pv.Name).Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimRetain {
			t.Errorf("PV reclaim policy = %s, want Retain (changed by the admin)", got)
		}

		if env.writes != writes {
			t.Errorf("second pass made %d writes, want 0", env.writes-writes)
		}

		if events := env.events(); len(events) != 0 {
			t.Errorf("events = %v, want none", events)
		}
	})

	// The policy of a volume of this version may be changed on purpose.
	t.Run("volume of this version is not touched", func(t *testing.T) {
		claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
		pv.Labels = map[string]string{provisioner.LabelManaged: provisioner.ValueTrue}

		env := newTestEnv(t, append(testClasses(), claim, pv)...)
		env.ctrl.fixReclaimPolicy = true

		env.migrate()

		if env.writes != 0 {
			t.Errorf("migration made %d writes, want 0", env.writes)
		}

		if events := env.events(); len(events) != 0 {
			t.Errorf("events = %v, want none", events)
		}
	})

	// A volume that was not provisioned by a backend of the class was not moved by v0.x.
	t.Run("static volume is not touched", func(t *testing.T) {
		for name, change := range map[string]func(pv *corev1.PersistentVolume){
			"hybrid class":   func(pv *corev1.PersistentVolume) { pv.Spec.StorageClassName = testHybridClass },
			"not dynamic":    func(pv *corev1.PersistentVolume) { delete(pv.Annotations, volume.AnnDynamicallyProvisioned) },
			"other provider": func(pv *corev1.PersistentVolume) { pv.Annotations[volume.AnnDynamicallyProvisioned] = "other" },
		} {
			t.Run(name, func(t *testing.T) {
				claim, pv := testLegacyBound(corev1.PersistentVolumeReclaimRetain)
				change(pv)

				env := newTestEnv(t, append(testClasses(), claim, pv)...)
				env.ctrl.fixReclaimPolicy = true

				env.migrate()

				if env.writes != 0 {
					t.Errorf("migration made %d writes, want 0", env.writes)
				}

				if events := env.events(); len(events) != 0 {
					t.Errorf("events = %v, want none", events)
				}
			})
		}
	})
}

func TestMigrateOrphans(t *testing.T) {
	deleted := testOrphan("pv-delete", nil, corev1.VolumeAvailable)
	deleted.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete

	static := testOrphan("pv-static", nil, corev1.VolumeAvailable)
	delete(static.Annotations, volume.AnnDynamicallyProvisioned)

	otherClass := testOrphan("pv-other-class", nil, corev1.VolumeAvailable)
	otherClass.Spec.StorageClassName = "standard"

	objs := []runtime.Object{
		// v0.x crashed after claimRef = null.
		testOrphan("pv-available", nil, corev1.VolumeAvailable),
		// v0.x helper deleted, the volume is Released from it.
		testOrphan("pv-released", &corev1.ObjectReference{Namespace: testNamespace, Name: legacyHelperName, UID: "uid-gone"}, corev1.VolumeReleased),

		// Not orphans.
		testOrphan("pv-released-user", &corev1.ObjectReference{Namespace: testNamespace, Name: "data", UID: "uid-gone"}, corev1.VolumeReleased),
		deleted,
		static,
		otherClass,
	}

	env := newTestEnv(t, append(testClasses(), objs...)...)

	env.migrate()

	if got := testutil.ToFloat64(env.ctrl.migrationMetrics.orphaned); got != 2 {
		t.Errorf("hybrid_orphaned_pv_total = %v, want 2", got)
	}

	if events := env.events(); len(events) != 2 {
		t.Errorf("events = %v, want 2 %s events", events, ReasonOrphanedVolume)
	}

	// Report only: never deleted or changed.
	if env.writes != 0 {
		t.Errorf("migration made %d writes, want 0", env.writes)
	}
}

func TestMigrateOrphanedHelpers(t *testing.T) {
	// The claim is gone, its v0.x helper is left.
	orphan := legacy(testHelper(testClaim()))
	orphan.Name = legacyHelperName
	orphan.UID = "uid-orphan"

	// The claim exists: Provision() adopts its helper.
	claim := testClaim()
	claim.Name = "web"
	claim.UID = "0a1b2c3d-0000-4000-8000-000000000002"
	pending := legacy(testHelper(claim))
	pending.UID = "uid-pending"

	// Adopted helper, its claim is gone: the lifecycle controller handles it.
	adopted := testHelper(testClaim())
	adopted.Name = "pvc-0a1b2c3d-0000-4000-8000-000000000003"
	adopted.UID = "uid-adopted"

	// A claim of another class, named like a helper.
	other := legacy(testHelper(testClaim()))
	other.Name = "pvc-0a1b2c3d-0000-4000-8000-000000000004"
	other.UID = "uid-other"
	other.Spec.StorageClassName = new("standard")

	env := newTestEnv(t, append(testClasses(), orphan, claim, pending, adopted, other)...)

	env.migrate()

	events := env.events()
	if len(events) != 1 || !strings.Contains(events[0], ReasonOrphanedHelper) {
		t.Errorf("events = %v, want one %s event", events, ReasonOrphanedHelper)
	}

	if got := testutil.ToFloat64(env.ctrl.migrationMetrics.orphanedHelpers); got != 1 {
		t.Errorf("hybrid_orphaned_helper_pvc_total = %v, want 1", got)
	}

	if env.writes != 0 {
		t.Errorf("migration made %d writes, want 0", env.writes)
	}
}

func TestMigrateLegacyPods(t *testing.T) {
	bound := testClaim()
	bound.Name = "pvc-0a1b2c3d-0000-4000-8000-000000000002"
	bound.Spec.VolumeName = "pv-backend"

	pending := testClaim()
	pending.Name = "pvc-0a1b2c3d-0000-4000-8000-000000000003"

	tests := []struct {
		name    string
		pod     *corev1.Pod
		deleted bool
	}{
		{name: "helper is bound", pod: testLegacyPod("provisioner-"+bound.Name, bound.Name), deleted: true},
		{name: "helper is gone", pod: testLegacyPod("provisioner-"+legacyHelperName, legacyHelperName), deleted: true},
		{name: "helper is pending", pod: testLegacyPod("provisioner-"+pending.Name, pending.Name)},
		{name: "other volume", pod: testLegacyPod("provisioner-"+legacyHelperName, "data")},
		{name: "another volume too", pod: testLegacyPod("provisioner-"+legacyHelperName, legacyHelperName, "data")},
		{name: "no volume", pod: testLegacyPod("provisioner-" + legacyHelperName)},
		{name: "other pod", pod: testLegacyPod("provisioner-web", "provisioner-web")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, append(testClasses(), bound, pending, tt.pod)...)

			env.migrate()

			_, err := env.client.CoreV1().Pods(testNamespace).Get(t.Context(), tt.pod.Name, metav1.GetOptions{})
			if deleted := apierrors.IsNotFound(err); deleted != tt.deleted {
				t.Errorf("pod deleted = %v (err %v), want %v", deleted, err, tt.deleted)
			}
		})
	}
}

// TestMigrateLegacyPodsDone: pods are listed until a pass finds no helper pods left.
func TestMigrateLegacyPodsDone(t *testing.T) {
	pending := testClaim()
	pending.Name = "pvc-0a1b2c3d-0000-4000-8000-000000000003"

	pod := testLegacyPod("provisioner-"+pending.Name, pending.Name)

	env := newTestEnv(t, append(testClasses(), pending, pod)...)

	podLists := func() int {
		n := 0

		for _, action := range env.client.Actions() {
			if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
				n++
			}
		}

		return n
	}

	// The helper is pending, its pod is kept and the next pass looks again.
	env.migrate()

	if env.ctrl.legacyPodsDone {
		t.Fatalf("legacy pods are done with a pending helper pod left")
	}

	if err := env.client.CoreV1().Pods(testNamespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete pod: %v", err)
	}

	env.migrate()

	if !env.ctrl.legacyPodsDone {
		t.Fatalf("legacy pods are not done without helper pods")
	}

	lists := podLists()

	env.migrate()

	if got := podLists(); got != lists {
		t.Errorf("pods listed %d more times after the migration is done, want 0", got-lists)
	}
}
