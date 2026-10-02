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

package framework

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// HybridDriverName is the provisioner name of hybrid StorageClasses.
const HybridDriverName = "csi.hybrid.sinextra.dev"

const (
	// ParamStorageClasses is the hybrid StorageClass parameter with the list of backend StorageClasses.
	ParamStorageClasses = "storageClasses"

	// FinalizerProvisioning is set on the user PVC while it is provisioned.
	FinalizerProvisioning = "csi.hybrid.sinextra.dev/provisioning"

	// AnnotationReschedules counts the reschedules of the user PVC caused by the helper timeout.
	AnnotationReschedules = "csi.hybrid.sinextra.dev/reschedules"
	// LabelManaged marks the PVs managed by the hybrid plugin, value "true".
	LabelManaged = "csi.hybrid.sinextra.dev/managed"
	// AnnotationMigrated marks the PVs provisioned by v0.x and adopted by the migration, value "true".
	AnnotationMigrated = "csi.hybrid.sinextra.dev/migrated"
	// AnnotationProvisionedBy is the annotation the backend provisioner sets on its PVs.
	AnnotationProvisionedBy = "pv.kubernetes.io/provisioned-by"

	// LabelRole marks helper PVCs, with the value LabelRoleHelper.
	LabelRole = "csi.hybrid.sinextra.dev/role"
	// LabelRoleHelper is the value of LabelRole on helper PVCs.
	LabelRoleHelper = "helper"
)

// HelperPVCName returns the name of the helper PVC of the user PVC.
func HelperPVCName(pvc *corev1.PersistentVolumeClaim) string {
	return "pvc-" + string(pvc.UID)
}

// LabeledHelperPVCs returns the PVCs in the namespace labeled as hybrid helpers.
func LabeledHelperPVCs(ctx context.Context, clientset *kubernetes.Clientset, namespace string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelRole + "=" + LabelRoleHelper,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list helper pvcs in %s: %w", namespace, err)
	}

	return list.Items, nil
}

// CreateStorageClass creates a cluster-scoped StorageClass for the test and deletes it on cleanup.
func (f *Framework) CreateStorageClass(sc *storagev1.StorageClass) error {
	f.T.Helper()

	ctx, cancel := f.Context()
	defer cancel()

	if _, err := f.Client.Clientset.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create storageclass %q: %w", sc.Name, err)
	}

	f.T.Cleanup(func() {
		ctx, cancel := f.Context()
		defer cancel()

		if err := f.Client.Clientset.StorageV1().StorageClasses().Delete(ctx, sc.Name, metav1.DeleteOptions{}); err != nil {
			f.Logf("failed to delete storageclass %s: %v", sc.Name, err)
		}
	})

	return nil
}

// BackendStorageClasses returns the hybrid StorageClass and its backend
// StorageClasses, in the parameters.storageClasses order. Every backend must
// exist in the cluster.
func BackendStorageClasses(ctx context.Context, clientset *kubernetes.Clientset, name string) (*storagev1.StorageClass, map[string]*storagev1.StorageClass, error) {
	hsc, err := clientset.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get storageclass %q: %w", name, err)
	}

	if hsc.Provisioner != HybridDriverName {
		return nil, nil, fmt.Errorf("storageclass %q has provisioner %q, want %q", name, hsc.Provisioner, HybridDriverName)
	}

	classes, ok := hsc.Parameters[ParamStorageClasses]
	if !ok || classes == "" {
		return nil, nil, fmt.Errorf("storageclass %q has no parameters.storageClasses", name)
	}

	backends := map[string]*storagev1.StorageClass{}

	for c := range strings.SplitSeq(classes, ",") {
		sc, err := clientset.StorageV1().StorageClasses().Get(ctx, c, metav1.GetOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get backend storageclass %q of %q: %w", c, name, err)
		}

		backends[c] = sc
	}

	return hsc, backends, nil
}

// HelperPVCs returns the PVCs in the namespace that do not use the hybrid
// StorageClass: helper PVCs the plugin created and did not clean up.
func HelperPVCs(ctx context.Context, clientset *kubernetes.Clientset, namespace, hybridClass string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pvcs in %s: %w", namespace, err)
	}

	var helpers []corev1.PersistentVolumeClaim

	for _, pvc := range list.Items {
		if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != hybridClass {
			helpers = append(helpers, pvc)
		}
	}

	return helpers, nil
}

// CordonNode marks the node unschedulable and uncordons it on cleanup.
func (f *Framework) CordonNode(name string) error {
	f.T.Helper()

	if err := f.setUnschedulable(name, true); err != nil {
		return err
	}

	f.T.Cleanup(func() {
		if err := f.setUnschedulable(name, false); err != nil {
			f.Logf("failed to uncordon node %s: %v", name, err)
		}
	})

	return nil
}

func (f *Framework) setUnschedulable(name string, unschedulable bool) error {
	ctx, cancel := f.Context()
	defer cancel()

	patch := fmt.Appendf(nil, `{"spec":{"unschedulable":%t}}`, unschedulable)

	if _, err := f.Client.Clientset.CoreV1().Nodes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to set node %s unschedulable=%t: %w", name, unschedulable, err)
	}

	return nil
}
