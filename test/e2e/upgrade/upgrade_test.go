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

package upgrade

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sergelogvinov/hybrid-csi-plugin/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	stsName = "upgrade"
	size    = "1Gi"

	stageBefore = "before"
	stageAfter  = "after"
)

// TestUpgrade runs in two stages, see hack/e2e-upgrade.sh:
//
//   - before: against the previous version deployed in the cluster (E2E_CONTROLLER=external),
//     creates a StatefulSet with one replica less than E2E_REPLICAS;
//   - after: against the new version, local or deployed by a helm upgrade, checks that the volumes of the previous version
//     are adopted and nothing is left over, scales the StatefulSet up (a volume of the new
//     version) and down, deletes it and checks that no volume is leaked.
func TestUpgrade(t *testing.T) {
	stage := framework.SharedConfig.UpgradeStage

	switch stage {
	case stageBefore, stageAfter:
	case "":
		t.Skip("not an upgrade run (E2E_UPGRADE_STAGE)")
	default:
		t.Fatalf("unknown E2E_UPGRADE_STAGE %q, want %q or %q", stage, stageBefore, stageAfter)
	}

	if stage == stageBefore && framework.SharedConfig.Controller != framework.ControllerExternal {
		t.Fatalf("stage %q runs against the previous version deployed in the cluster, set E2E_CONTROLLER=%s",
			stageBefore, framework.ControllerExternal)
	}

	f := framework.NewInNamespace(t, framework.SharedConfig.UpgradeNamespace)

	replicas := f.Config.Replicas
	require.GreaterOrEqual(t, replicas, int32(2), "E2E_REPLICAS must be at least 2")

	if stage == stageBefore {
		createStatefulSet(t, f, replicas-1)

		return
	}

	verifyAdopted(t, f, replicas-1)
	scaleAndDelete(t, f, replicas)
}

func createStatefulSet(t *testing.T, f *framework.Framework, replicas int32) {
	t.Helper()

	require := require.New(t)

	sts := framework.NewTestStatefulSet(framework.StatefulSetOptions{
		Name:         stsName,
		Namespace:    f.Namespace,
		StorageClass: f.Config.StorageClass,
		Replicas:     replicas,
		Size:         size,
	})

	ctx, cancel := f.Context()
	_, err := f.Client.Clientset.AppsV1().StatefulSets(f.Namespace).Create(ctx, sts, metav1.CreateOptions{})

	cancel()
	require.NoError(err, "failed to create statefulset")

	done := f.Step("waiting for statefulset %s to have %d ready replicas", stsName, replicas)
	ctx, cancel = f.Context()
	_, err = framework.WaitForStatefulSetReplicasReady(ctx, f.Client.Clientset, f.Namespace, stsName, replicas, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)
}

// verifyAdopted checks the volumes provisioned by the previous version.
func verifyAdopted(t *testing.T, f *framework.Framework, replicas int32) {
	t.Helper()

	require := require.New(t)

	for i := range int(replicas) {
		pvcName := framework.StatefulSetPVCName(stsName, i)

		ctx, cancel := f.Context()
		pvc, pv, err := framework.WaitForPVCBound(ctx, f.Client.Clientset, f.Namespace, pvcName, f.Config.Timeout)

		cancel()
		require.NoError(err)

		// The migration pass runs when the upgraded controller starts.
		done := f.Step("waiting for pv %s to be adopted", pv.Name)
		ctx, cancel = f.Context()
		err = wait.PollUntilContextTimeout(ctx, framework.PollInterval, f.Config.Timeout, true, func(ctx context.Context) (bool, error) {
			pv, err = f.Client.Clientset.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
			if err != nil {
				return false, nil //nolint:nilerr // retried until the timeout
			}

			return pv.Labels[framework.LabelManaged] == "true" && pv.Annotations[framework.AnnotationMigrated] == "true", nil
		})

		cancel()
		done()
		require.NoError(err, "pv %s of pvc %s is not adopted: %v %v", pv.Name, pvcName, pv.Labels, pv.Annotations)

		require.Equal(pvc.UID, pv.Spec.ClaimRef.UID, "pv %s is not bound to pvc %s", pv.Name, pvcName)
		require.NotContains(pvc.Finalizers, framework.FinalizerProvisioning)
	}

	// Nothing of the previous version is left over.
	ctx, cancel := f.Context()
	helpers, err := framework.HelperPVCs(ctx, f.Client.Clientset, f.Namespace, f.Config.StorageClass)

	cancel()
	require.NoError(err)
	require.Empty(helpers, "helper pvcs left behind")

	ctx, cancel = f.Context()
	pods, err := f.Client.Clientset.CoreV1().Pods(f.Namespace).List(ctx, metav1.ListOptions{})

	cancel()
	require.NoError(err)

	for _, pod := range pods.Items {
		require.False(strings.HasPrefix(pod.Name, "provisioner-pvc-"), "helper pod %s left behind", pod.Name)
	}
}

// scaleAndDelete scales the StatefulSet up and down, deletes it with its PVCs and the
// namespace, and checks that no volume is leaked.
func scaleAndDelete(t *testing.T, f *framework.Framework, replicas int32) {
	t.Helper()

	require := require.New(t)

	// Scale up: the new replica gets a volume of the new version.
	scale(t, f, replicas)

	newPVC := framework.StatefulSetPVCName(stsName, int(replicas)-1)

	ctx, cancel := f.Context()
	_, pv, err := framework.WaitForPVCBound(ctx, f.Client.Clientset, f.Namespace, newPVC, f.Config.Timeout)

	cancel()
	require.NoError(err)
	require.Equal("true", pv.Labels[framework.LabelManaged], "pv %s of the new version is not managed", pv.Name)
	require.Empty(pv.Annotations[framework.AnnotationMigrated], "pv %s of the new version is marked as migrated", pv.Name)

	// Scale down to zero, then delete everything.
	scale(t, f, 0)

	for i := range int(replicas) {
		ctx, cancel := f.Context()
		err = framework.WaitForPodGone(ctx, f.Client.Clientset, f.Namespace, framework.StatefulSetPodName(stsName, i), f.Config.Timeout)

		cancel()
		require.NoError(err)
	}

	pvNames := make([]string, 0, replicas)

	for i := range int(replicas) {
		pvcName := framework.StatefulSetPVCName(stsName, i)

		ctx, cancel := f.Context()
		pvc, err := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, pvcName, metav1.GetOptions{})

		cancel()
		require.NoError(err)

		pvNames = append(pvNames, pvc.Spec.VolumeName)
	}

	ctx, cancel = f.Context()
	err = f.Client.Clientset.AppsV1().StatefulSets(f.Namespace).Delete(ctx, stsName, metav1.DeleteOptions{})

	cancel()
	require.NoError(err)

	f.Logf("deleting namespace %s", f.Namespace)

	ctx, cancel = f.Context()
	err = f.Client.Clientset.CoreV1().Namespaces().Delete(ctx, f.Namespace, metav1.DeleteOptions{})

	cancel()
	require.NoError(err)

	done := f.Step("waiting for namespace %s to be deleted", f.Namespace)
	ctx, cancel = f.Context()
	err = framework.WaitForNamespaceGone(ctx, f.Client.Clientset, f.Namespace, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)

	for _, name := range pvNames {
		ctx, cancel := f.Context()
		pv, err := f.Client.Clientset.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})

		cancel()

		if err == nil && pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			f.Logf("pv %s has reclaimPolicy %s, it is kept", name, pv.Spec.PersistentVolumeReclaimPolicy)

			continue
		}

		done = f.Step("waiting for pv %s to be deleted", name)
		ctx, cancel = f.Context()
		err = framework.WaitForPVGone(ctx, f.Client.Clientset, name, f.Config.Timeout)

		cancel()
		done()
		require.NoError(err)
	}
}

func scale(t *testing.T, f *framework.Framework, replicas int32) {
	t.Helper()

	require := require.New(t)

	f.Logf("scaling statefulset %s to %d", stsName, replicas)

	ctx, cancel := f.Context()
	_, err := f.Client.Clientset.AppsV1().StatefulSets(f.Namespace).Patch(ctx, stsName, types.MergePatchType,
		fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas), metav1.PatchOptions{})

	cancel()
	require.NoError(err)

	done := f.Step("waiting for statefulset %s to have %d ready replicas", stsName, replicas)
	ctx, cancel = f.Context()
	_, err = framework.WaitForStatefulSetReplicasReady(ctx, f.Client.Clientset, f.Namespace, stsName, replicas, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)
}
