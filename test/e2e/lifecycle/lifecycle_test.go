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

package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sergelogvinov/hybrid-csi-plugin/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	annotationSelectedNode = "volume.kubernetes.io/selected-node"

	// helperCleanupTimeout is shorter than the cleanup delay of the lifecycle controller (30s).
	helperCleanupTimeout = 10 * time.Second
)

// TestStatefulSetLifecycle deploys a StatefulSet on the hybrid StorageClass,
// waits for it to be healthy, checks that every volume was provisioned by a
// backend from the hybrid class and moved to the user PVC, then deletes it
// and confirms the volumes are gone and no helper PVC was left behind.
func TestStatefulSetLifecycle(t *testing.T) {
	f := framework.New(t)
	require := require.New(t)

	const (
		stsName = "lifecycle"
		size    = "1Gi"
	)

	replicas := f.Config.Replicas

	stsClient := f.Client.Clientset.AppsV1().StatefulSets(f.Namespace)
	pvcClient := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace)

	// 1. The hybrid StorageClass and its backends must already exist.
	ctx, cancel := f.Context()
	hsc, backends, err := framework.BackendStorageClasses(ctx, f.Client.Clientset, f.Config.StorageClass)

	cancel()
	require.NoError(err)
	require.NotNil(hsc.VolumeBindingMode, "storageclass %s has no volumeBindingMode set", hsc.Name)
	require.Equal(storagev1.VolumeBindingWaitForFirstConsumer, *hsc.VolumeBindingMode,
		"storageclass %s must use WaitForFirstConsumer", hsc.Name)
	f.Logf("storageclass %s, backends %s", hsc.Name, hsc.Parameters[framework.ParamStorageClasses])

	// 2. Create the StatefulSet.
	f.Logf("creating statefulset %s (storageClass=%s size=%s replicas=%d)", stsName, hsc.Name, size, replicas)

	sts := framework.NewTestStatefulSet(framework.StatefulSetOptions{
		Name:         stsName,
		Namespace:    f.Namespace,
		StorageClass: hsc.Name,
		Replicas:     replicas,
		Size:         size,
	})

	ctx, cancel = f.Context()
	_, err = stsClient.Create(ctx, sts, metav1.CreateOptions{})

	cancel()
	require.NoError(err, "failed to create statefulset")

	done := f.Step("waiting for statefulset %s to have %d ready replicas", stsName, replicas)
	ctx, cancel = f.Context()
	_, err = framework.WaitForStatefulSetReplicasReady(ctx, f.Client.Clientset, f.Namespace, stsName, replicas, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)

	// 3. Every replica: PVC bound to a backend PV that serves the pod's node.
	pvNames := make([]string, 0, replicas)
	used := map[string]int{}

	for i := range int(replicas) {
		podName := framework.StatefulSetPodName(stsName, i)
		pvcName := framework.StatefulSetPVCName(stsName, i)

		done = f.Step("waiting for pvc %s to bind", pvcName)
		ctx, cancel := f.Context()
		pvc, pv, err := framework.WaitForPVCBound(ctx, f.Client.Clientset, f.Namespace, pvcName, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)

		pvNames = append(pvNames, pv.Name)
		require.NotContains(pvNames[:len(pvNames)-1], pv.Name, "two replicas are bound to the same pv %s", pv.Name)

		backend, ok := backends[pv.Spec.StorageClassName]
		require.True(ok, "pv %s has storageclass %q, want one of the backends %s",
			pv.Name, pv.Spec.StorageClassName, hsc.Parameters[framework.ParamStorageClasses])

		used[backend.Name]++

		require.Equal(backend.Provisioner, pv.Annotations[framework.AnnotationProvisionedBy],
			"pv %s must stay owned by the backend provisioner", pv.Name)

		if pv.Spec.CSI != nil {
			require.Equal(backend.Provisioner, pv.Spec.CSI.Driver, "pv %s csi driver", pv.Name)
		}

		require.NotNil(pv.Spec.ClaimRef, "pv %s has no claimRef", pv.Name)
		require.Equal(pvc.UID, pv.Spec.ClaimRef.UID, "pv %s is bound to %s/%s, want pvc %s",
			pv.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, pvcName)

		ctx, cancel = f.Context()
		pod, err := f.Client.Clientset.CoreV1().Pods(f.Namespace).Get(ctx, podName, metav1.GetOptions{})

		cancel()
		require.NoError(err)
		require.Equal(pod.Spec.NodeName, pvc.Annotations[annotationSelectedNode],
			"pvc %s was provisioned for another node", pvcName)

		if pv.Spec.NodeAffinity != nil {
			ctx, cancel = f.Context()
			node, err := f.Client.Clientset.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})

			cancel()
			require.NoError(err)
			require.True(framework.NodeMatchesVolumeNodeAffinity(pv.Spec.NodeAffinity, node),
				"pv %s nodeAffinity does not match node %s", pv.Name, node.Name)
		}

		f.Logf("pod %s on node %s: pvc %s bound to pv %s (backend %s, reclaimPolicy %s)",
			podName, pod.Spec.NodeName, pvcName, pv.Name, backend.Name, pv.Spec.PersistentVolumeReclaimPolicy)
	}

	f.Logf("backends used: %v", used)

	// 4. No helper PVC is left in the namespace. Provision() deletes it right after binding the
	// user PVC, well before the lifecycle controller would clean it up as a leftover.
	var helpers []corev1.PersistentVolumeClaim

	done = f.Step("waiting for the helper pvcs to be deleted")
	ctx, cancel = f.Context()
	err = wait.PollUntilContextTimeout(ctx, time.Second, helperCleanupTimeout, true, func(ctx context.Context) (bool, error) {
		list, err := framework.HelperPVCs(ctx, f.Client.Clientset, f.Namespace, hsc.Name)
		if err != nil {
			return false, nil //nolint:nilerr // retried until the timeout
		}

		helpers = list

		return len(helpers) == 0, nil
	})

	cancel()
	done()
	require.NoError(err, "helper pvcs left behind: %v", helperNames(helpers))

	// 5. Delete the StatefulSet and its PVCs: volumes with the Delete policy must go away.
	f.Logf("deleting statefulset %s and its pvcs", stsName)

	ctx, cancel = f.Context()
	err = stsClient.Delete(ctx, stsName, metav1.DeleteOptions{})

	cancel()
	require.NoError(err, "failed to delete statefulset")

	for i := range int(replicas) {
		podName := framework.StatefulSetPodName(stsName, i)
		pvcName := framework.StatefulSetPVCName(stsName, i)

		done = f.Step("waiting for pod %s to be deleted", podName)
		ctx, cancel := f.Context()
		err = framework.WaitForPodGone(ctx, f.Client.Clientset, f.Namespace, podName, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)

		ctx, cancel = f.Context()
		err = pvcClient.Delete(ctx, pvcName, metav1.DeleteOptions{})

		cancel()
		require.NoError(err, "failed to delete pvc %s", pvcName)

		done = f.Step("waiting for pvc %s to be deleted", pvcName)
		ctx, cancel = f.Context()
		err = framework.WaitForPVCGone(ctx, f.Client.Clientset, f.Namespace, pvcName, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)
	}

	for _, pvName := range pvNames {
		ctx, cancel := f.Context()
		pv, err := f.Client.Clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})

		cancel()

		if err == nil && pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			f.Logf("pv %s has reclaimPolicy %s, it is kept", pvName, pv.Spec.PersistentVolumeReclaimPolicy)

			continue
		}

		done = f.Step("waiting for pv %s to be deleted", pvName)
		ctx, cancel = f.Context()
		err = framework.WaitForPVGone(ctx, f.Client.Clientset, pvName, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)
	}
}

func helperNames(pvcs []corev1.PersistentVolumeClaim) []string {
	names := make([]string, 0, len(pvcs))
	for _, pvc := range pvcs {
		names = append(names, pvc.Name)
	}

	return names
}
