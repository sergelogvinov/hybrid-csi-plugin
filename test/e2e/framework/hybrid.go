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
	"k8s.io/client-go/kubernetes"
)

// HybridDriverName is the provisioner name of hybrid StorageClasses.
const HybridDriverName = "csi.hybrid.sinextra.dev"

// AnnProvisionedBy is the annotation the backend provisioner sets on its PVs.
const AnnProvisionedBy = "pv.kubernetes.io/provisioned-by"

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

	classes, ok := hsc.Parameters["storageClasses"]
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
