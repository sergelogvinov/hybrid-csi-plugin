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
	"fmt"
	"time"

	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

const (
	helperBindTimeout = 30 * time.Second
	helperPodImage    = "registry.k8s.io/pause:3.10"
)

// helperName returns the name of the helper PVC (H) for the provision request.
func helperName(opts controller.ProvisionOptions) string {
	return opts.PVName
}

// helperPodName returns the name of the helper pod for the provision request (pod method).
func helperPodName(opts controller.ProvisionOptions) string {
	return fmt.Sprintf("provisioner-%s", opts.PVName)
}

// buildHelperPVC builds the helper PVC (H) for the backend StorageClass.
func buildHelperPVC(opts controller.ProvisionOptions, storageClass *storagev1.StorageClass, withSelectedNode bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperName(opts),
			Namespace: opts.PVC.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      opts.PVC.Spec.AccessModes,
			StorageClassName: &storageClass.Name,
			Resources:        opts.PVC.Spec.Resources,
			VolumeMode:       opts.PVC.Spec.VolumeMode,
		},
	}

	if withSelectedNode {
		pvc.Annotations = map[string]string{
			annStorageProvisioner:     storageClass.Provisioner,
			annBetaStorageProvisioner: storageClass.Provisioner,
			annSelectedNode:           opts.SelectedNode.Name,
		}
	}

	return pvc
}

// buildHelperPod builds the pause pod that consumes the helper PVC on the selected node (pod method).
func buildHelperPod(opts controller.ProvisionOptions, helper *corev1.PersistentVolumeClaim) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperPodName(opts),
			Namespace: helper.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "provisioner",
					Image: helperPodImage,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("10Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("10Mi"),
						},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			NodeSelector: map[string]string{
				corev1.LabelHostname: opts.SelectedNode.Name,
			},
			Volumes: []corev1.Volume{
				{
					Name:         "provisioner",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: helper.Name}},
				},
			},
		},
	}
}

// ensureHelperPVC creates the helper PVC if it does not exist yet.
func (p *HybridProvisioner) ensureHelperPVC(ctx context.Context, helper *corev1.PersistentVolumeClaim) error {
	if _, err := p.claimLister.PersistentVolumeClaims(helper.Namespace).Get(helper.Name); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get persistentvolumeclaim: %v", err)
		}

		_, err = p.client.CoreV1().PersistentVolumeClaims(helper.Namespace).Create(ctx, helper, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create persistentvolumeclaim: %v", err)
		}
	}

	return nil
}

// waitBindPVC waits until the helper PVC is bound by the backend provisioner.
func (p *HybridProvisioner) waitBindPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	watcher, err := p.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + pvc.Name,
	})
	if err != nil {
		return nil, err
	}

	defer watcher.Stop()

	timeout := time.After(p.bindTimeout)

	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch channel closed unexpectedly")
			}

			if event.Type == watch.Modified {
				obj, ok := event.Object.(*corev1.PersistentVolumeClaim)
				if ok && obj.Status.Phase == corev1.ClaimBound {
					klog.V(4).InfoS("helper persistent volume claim is bound", "PVC", klog.KObj(obj), "PV", obj.Spec.VolumeName)

					return obj, nil
				}
			}

		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for PersistentVolumeClaims %s to be boned", pvc.Name)
		}
	}
}
