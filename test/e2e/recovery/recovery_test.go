//go:build e2e

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

package recovery

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sergelogvinov/hybrid-csi-plugin/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	size = "1Gi"

	// faultAfterMove is the controller fault injection point right after the volume is moved to the claim.
	faultAfterMove = "after-move"
)

// TestClaimDeletedWhileHelperPending deletes the user PVC while its helper waits
// for a backend that never provisions: the helper is deleted and the claim goes away.
func TestClaimDeletedWhileHelperPending(t *testing.T) {
	f := framework.New(t)
	require := require.New(t)

	// A backend with a provisioner that does not exist, and a hybrid class on top of it.
	backend := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: f.Namespace + "-never"},
		Provisioner:       "never.e2e.invalid",
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	hybrid := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: f.Namespace + "-hybrid"},
		Provisioner:       framework.HybridDriverName,
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
		Parameters:        map[string]string{framework.ParamStorageClasses: backend.Name},
	}

	require.NoError(f.CreateStorageClass(backend))
	require.NoError(f.CreateStorageClass(hybrid))

	pvcClient := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace)
	podClient := f.Client.Clientset.CoreV1().Pods(f.Namespace)

	ctx, cancel := f.Context()
	pvc, err := pvcClient.Create(ctx, framework.NewTestPVC(f.Namespace, "data", hybrid.Name, size), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	ctx, cancel = f.Context()
	_, err = podClient.Create(ctx, framework.NewTestPod(f.Namespace, "consumer", pvc.Name), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	done := f.Step("waiting for the helper of pvc %s", pvc.Name)
	ctx, cancel = f.Context()
	helper, err := framework.WaitForHelperPVC(ctx, f.Client.Clientset, pvc, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)
	require.Equal(backend.Name, *helper.Spec.StorageClassName)

	ctx, cancel = f.Context()
	pvc, err = pvcClient.Get(ctx, pvc.Name, metav1.GetOptions{})

	cancel()
	require.NoError(err)
	require.Contains(pvc.Finalizers, framework.FinalizerProvisioning, "pvc %s has no provisioning finalizer", pvc.Name)

	// Delete the claim while the helper is pending.
	f.Logf("deleting pod and pvc %s", pvc.Name)

	ctx, cancel = f.Context()
	err = podClient.Delete(ctx, "consumer", metav1.DeleteOptions{})

	cancel()
	require.NoError(err)

	ctx, cancel = f.Context()
	err = pvcClient.Delete(ctx, pvc.Name, metav1.DeleteOptions{})

	cancel()
	require.NoError(err)

	for _, name := range []string{pvc.Name, helper.Name} {
		done = f.Step("waiting for pvc %s to be deleted", name)
		ctx, cancel := f.Context()
		err = framework.WaitForPVCGone(ctx, f.Client.Clientset, f.Namespace, name, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)
	}
}

// TestNamespaceDeletedDuringProvisioning deletes a namespace while 10 PVCs are
// being provisioned: the namespace goes away and no volume of it is left behind.
func TestNamespaceDeletedDuringProvisioning(t *testing.T) {
	f := framework.New(t)
	require := require.New(t)

	const claims = 10

	pvcClient := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace)
	podClient := f.Client.Clientset.CoreV1().Pods(f.Namespace)

	created := make([]*corev1.PersistentVolumeClaim, 0, claims)

	for i := range claims {
		name := fmt.Sprintf("data-%d", i)

		ctx, cancel := f.Context()
		pvc, err := pvcClient.Create(ctx, framework.NewTestPVC(f.Namespace, name, f.Config.StorageClass, size), metav1.CreateOptions{})

		cancel()
		require.NoError(err)

		ctx, cancel = f.Context()
		_, err = podClient.Create(ctx, framework.NewTestPod(f.Namespace, fmt.Sprintf("consumer-%d", i), name), metav1.CreateOptions{})

		cancel()
		require.NoError(err)

		created = append(created, pvc)
	}

	// Provisioning is in progress once the first helper exists.
	done := f.Step("waiting for the first helper pvc")
	ctx, cancel := f.Context()
	_, err := framework.WaitForHelperPVC(ctx, f.Client.Clientset, created[0], f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)

	f.Logf("deleting namespace %s", f.Namespace)

	ctx, cancel = f.Context()
	err = f.Client.Clientset.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{})

	cancel()
	require.NoError(err)

	done = f.Step("waiting for namespace %s to be deleted", f.Namespace)
	ctx, cancel = f.Context()
	err = framework.WaitForNamespaceGone(ctx, f.Client.Clientset, f.Namespace, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err, "namespace is stuck, finalizers were not released")

	// Every volume of the namespace is reclaimed. Volumes of helpers are always
	// deleted, volumes of claims follow the reclaim policy of the hybrid class.
	helpers := map[string]bool{}
	for _, pvc := range created {
		helpers[framework.HelperPVCName(pvc)] = true
	}

	ctx, cancel = f.Context()
	pvs, err := f.Client.Clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})

	cancel()
	require.NoError(err)

	for _, pv := range pvs.Items {
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != f.Namespace {
			continue
		}

		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			require.False(helpers[pv.Spec.ClaimRef.Name], "pv %s of helper %s is not deleted, reclaim policy %s",
				pv.Name, pv.Spec.ClaimRef.Name, pv.Spec.PersistentVolumeReclaimPolicy)
			f.Logf("pv %s of pvc %s has reclaimPolicy %s, it is kept", pv.Name, pv.Spec.ClaimRef.Name, pv.Spec.PersistentVolumeReclaimPolicy)

			continue
		}

		done = f.Step("waiting for pv %s of %s to be deleted", pv.Name, pv.Spec.ClaimRef.Name)
		ctx, cancel := f.Context()
		err = framework.WaitForPVGone(ctx, f.Client.Clientset, pv.Name, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)
	}
}

// TestCrashAfterMove runs a controller with --fault-inject=after-move: it exits right
// after the volume is moved, when the claim is not bound yet. After the restart
// the provisioning is finished and nothing is left behind.
func TestCrashAfterMove(t *testing.T) {
	framework.RequireLocalController(t)

	f := framework.New(t, framework.WithControllerArgs("--fault-inject="+faultAfterMove))
	require := require.New(t)

	pvcClient := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace)

	ctx, cancel := f.Context()
	pvc, err := pvcClient.Create(ctx, framework.NewTestPVC(f.Namespace, "data", f.Config.StorageClass, size), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	ctx, cancel = f.Context()
	_, err = f.Client.Clientset.CoreV1().Pods(f.Namespace).Create(ctx, framework.NewTestPod(f.Namespace, "consumer", pvc.Name), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	done := f.Step("waiting for pod consumer to be ready")
	ctx, cancel = f.Context()
	_, err = framework.WaitForPodReady(ctx, f.Client.Clientset, f.Namespace, "consumer", f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)

	ctx, cancel = f.Context()
	pvc, pv, err := framework.WaitForPVCBound(ctx, f.Client.Clientset, f.Namespace, pvc.Name, f.Config.Timeout)

	cancel()
	require.NoError(err)
	require.NotNil(pv.Spec.ClaimRef)
	require.Equal(pvc.UID, pv.Spec.ClaimRef.UID, "pv %s is not bound to pvc %s", pv.Name, pvc.Name)
	require.Positive(f.Controller.Restarts(), "the controller did not exit at %s", faultAfterMove)

	// The helper and the finalizer are cleaned up by the restarted controller.
	done = f.Step("waiting for helper %s to be deleted", framework.HelperPVCName(pvc))
	ctx, cancel = f.Context()
	err = framework.WaitForPVCGone(ctx, f.Client.Clientset, f.Namespace, framework.HelperPVCName(pvc), f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)

	ctx, cancel = f.Context()
	pvc, err = pvcClient.Get(ctx, pvc.Name, metav1.GetOptions{})

	cancel()
	require.NoError(err)
	require.NotContains(pvc.Finalizers, framework.FinalizerProvisioning)

	// Exactly one volume was provisioned for the claim.
	ctx, cancel = f.Context()
	pvs, err := f.Client.Clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})

	cancel()
	require.NoError(err)

	volumes := slices.DeleteFunc(pvs.Items, func(pv corev1.PersistentVolume) bool {
		return pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != f.Namespace
	})
	require.Len(volumes, 1, "volumes of namespace %s: %v", f.Namespace, volumeNames(volumes))
}

func volumeNames(pvs []corev1.PersistentVolume) string {
	names := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		names = append(names, pv.Name)
	}

	return strings.Join(names, ", ")
}
