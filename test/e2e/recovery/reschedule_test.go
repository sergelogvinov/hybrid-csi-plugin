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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sergelogvinov/hybrid-csi-plugin/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	reasonRescheduled = "Rescheduled"

	// neverProvisioner is a provisioner that does not exist: its PVCs stay pending.
	neverProvisioner = "never.e2e.invalid"

	// helperTimeout is the --helper-timeout of the controller, short to keep the tests fast.
	helperTimeout = time.Minute
)

// newRescheduleFramework creates a Framework with a controller that has a short helper timeout.
func newRescheduleFramework(t *testing.T) *framework.Framework {
	t.Helper()

	framework.RequireLocalController(t)

	return framework.New(t, framework.WithControllerArgs("--helper-timeout="+helperTimeout.String()))
}

// TestBackendNeverProvisions: the only backend never provisions the helper,
// the claim is rescheduled after the helper timeout.
func TestBackendNeverProvisions(t *testing.T) {
	f := newRescheduleFramework(t)
	require := require.New(t)

	backend := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: f.Namespace + "-never"},
		Provisioner:       neverProvisioner,
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

	ctx, cancel := f.Context()
	pvc, err := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace).Create(ctx,
		framework.NewTestPVC(f.Namespace, "data", hybrid.Name, size), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	ctx, cancel = f.Context()
	_, err = f.Client.Clientset.CoreV1().Pods(f.Namespace).Create(ctx, framework.NewTestPod(f.Namespace, "consumer", pvc.Name), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	timeout := helperTimeout + f.Config.Timeout

	done := f.Step("waiting for pvc %s to be rescheduled after the helper timeout %s", pvc.Name, helperTimeout)
	ctx, cancel = context.WithTimeout(context.Background(), timeout)
	event, err := framework.WaitForEvent(ctx, f.Client.Clientset, f.Namespace, pvc.Name, reasonRescheduled, timeout)

	cancel()
	done()
	require.NoError(err)
	f.Logf("event %s: %s", event.Reason, event.Message)

	ctx, cancel = f.Context()
	pvc, err = f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})

	cancel()
	require.NoError(err)
	require.NotEmpty(pvc.Annotations[framework.AnnotationReschedules], "pvc %s has no reschedule count", pvc.Name)
	require.Empty(pvc.Spec.VolumeName)
}

// TestNodeCordonedAfterHelperCreated: the claim is placed on a node whose backend never
// provisions, the node is cordoned, and after the helper timeout the claim is rescheduled
// to another node and provisioned there by a real backend.
func TestNodeCordonedAfterHelperCreated(t *testing.T) {
	f := newRescheduleFramework(t)
	require := require.New(t)

	ctx, cancel := f.Context()
	hsc, _, err := framework.BackendStorageClasses(ctx, f.Client.Clientset, f.Config.StorageClass)

	cancel()
	require.NoError(err)

	// The node X: the first schedulable node.
	ctx, cancel = f.Context()
	nodes, err := f.Client.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})

	cancel()
	require.NoError(err)

	var nodeName string

	for _, node := range nodes.Items {
		if !node.Spec.Unschedulable {
			nodeName = node.Name

			break
		}
	}

	require.NotEmpty(nodeName, "no schedulable node")
	require.Greater(len(nodes.Items), 1, "the test needs at least 2 nodes")

	// A backend that never provisions, only on node X, before the real backends.
	never := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: f.Namespace + "-never"},
		Provisioner:       neverProvisioner,
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
		AllowedTopologies: []corev1.TopologySelectorTerm{{
			MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: corev1.LabelHostname, Values: []string{nodeName}}},
		}},
	}
	hybrid := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: f.Namespace + "-hybrid"},
		Provisioner:       framework.HybridDriverName,
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
		ReclaimPolicy:     new(corev1.PersistentVolumeReclaimDelete),
		Parameters:        map[string]string{framework.ParamStorageClasses: strings.Join([]string{never.Name, hsc.Parameters[framework.ParamStorageClasses]}, ",")},
	}

	require.NoError(f.CreateStorageClass(never))
	require.NoError(f.CreateStorageClass(hybrid))

	ctx, cancel = f.Context()
	pvc, err := f.Client.Clientset.CoreV1().PersistentVolumeClaims(f.Namespace).Create(ctx,
		framework.NewTestPVC(f.Namespace, "data", hybrid.Name, size), metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	// The pod prefers node X, so the claim is placed there first.
	pod := framework.NewTestPod(f.Namespace, "consumer", pvc.Name)
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 100,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{nodeName},
					}},
				},
			}},
		},
	}

	ctx, cancel = f.Context()
	_, err = f.Client.Clientset.CoreV1().Pods(f.Namespace).Create(ctx, pod, metav1.CreateOptions{})

	cancel()
	require.NoError(err)

	done := f.Step("waiting for the helper of pvc %s on node %s", pvc.Name, nodeName)
	ctx, cancel = f.Context()
	helper, err := framework.WaitForHelperPVC(ctx, f.Client.Clientset, pvc, f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)
	require.Equal(never.Name, *helper.Spec.StorageClassName, "the helper does not use the never-provisioning backend")
	require.Equal(nodeName, helper.Annotations["volume.kubernetes.io/selected-node"])

	f.Logf("cordoning node %s", nodeName)
	require.NoError(f.CordonNode(nodeName))

	done = f.Step("waiting for pod consumer to be ready after the helper timeout %s", helperTimeout)
	ctx, cancel = context.WithTimeout(context.Background(), helperTimeout+f.Config.Timeout)
	pod, err = framework.WaitForPodReady(ctx, f.Client.Clientset, f.Namespace, "consumer", helperTimeout+f.Config.Timeout)

	cancel()
	done()
	require.NoError(err)
	require.NotEqual(nodeName, pod.Spec.NodeName, "pod is running on the cordoned node")

	ctx, cancel = f.Context()
	pvc, pv, err := framework.WaitForPVCBound(ctx, f.Client.Clientset, f.Namespace, pvc.Name, f.Config.Timeout)

	cancel()
	require.NoError(err)
	require.NotEqual(never.Name, pv.Spec.StorageClassName)
	require.Equal("1", pvc.Annotations[framework.AnnotationReschedules])

	f.Logf("pvc %s rescheduled to node %s, bound to pv %s of %s", pvc.Name, pod.Spec.NodeName, pv.Name, pv.Spec.StorageClassName)
}
