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
	"maps"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// managedByLabel/managedByValue mark namespaces created by the suite,
	// so SweepLeftoverNamespaces can find leftovers of a killed run.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "hybrid-csi-plugin-e2e"

	// appLabelKey is the label used to select a StatefulSet's own pods, both for
	// its Selector and for the anti-affinity term spreading replicas across nodes.
	appLabelKey = "app"

	alpineImage       = "alpine"
	storageVolumeName = "storage"
	storageMountPath  = "/mnt"
)

// newStorageContainer builds a sleeping alpine container (so a test can exec
// into it while its volume is mounted) mounting volumeName at storageMountPath.
func newStorageContainer(runAsUser int64, volumeName string) corev1.Container {
	return corev1.Container{
		Name:    alpineImage,
		Image:   alpineImage,
		Command: []string{"sleep", "1d"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			RunAsUser:                new(runAsUser),
			RunAsGroup:               new(runAsUser),
			RunAsNonRoot:             new(true),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeName, MountPath: storageMountPath},
		},
	}
}

// StatefulSetOptions parameterizes NewTestStatefulSet.
type StatefulSetOptions struct {
	Name         string
	Namespace    string
	StorageClass string
	Replicas     int32
	Size         string // e.g. "1Gi"
	Labels       map[string]string
}

// NewTestStatefulSet builds a StatefulSet + PVC-per-pod object mirroring
// docs/deploy/test-statefulset.yaml, parameterized for the e2e suite: an
// alpine container sleeping with a single volume mounted at /mnt, one PVC
// per replica via volumeClaimTemplates, and pod anti-affinity spreading
// replicas across nodes (so they can land on nodes served by different backends).
func NewTestStatefulSet(opts StatefulSetOptions) *appsv1.StatefulSet {
	labels := map[string]string{}

	maps.Copy(labels, opts.Labels)
	labels[appLabelKey] = opts.Name

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			PodManagementPolicy: appsv1.ParallelPodManagement,
			ServiceName:         opts.Name,
			Replicas:            &opts.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{appLabelKey: opts.Name},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64(3)),
					Tolerations: []corev1.Toleration{
						{Effect: corev1.TaintEffectNoSchedule, Key: "node-role.kubernetes.io/control-plane"},
					},
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: corev1.LabelHostname,
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      appLabelKey,
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{opts.Name},
											},
										},
									},
								},
							},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:    new(int64(1000)),
						RunAsUser:  new(int64(1000)),
						RunAsGroup: new(int64(1000)),
					},
					Containers: []corev1.Container{newStorageContainer(1000, storageVolumeName)},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: storageVolumeName,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: &opts.StorageClass,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(opts.Size),
							},
						},
					},
				},
			},
		},
	}
}

// NewNamespace builds a Namespace object labeled as belonging to the e2e suite.
func NewNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
	}
}

// StatefulSetPVCName returns the name of the PVC Kubernetes generates for a
// given StatefulSet ordinal, e.g. "storage-test-0".
func StatefulSetPVCName(stsName string, ordinal int) string {
	return storageVolumeName + "-" + stsName + "-" + strconv.Itoa(ordinal)
}

// StatefulSetPodName returns the name of the pod of a given StatefulSet ordinal.
func StatefulSetPodName(stsName string, ordinal int) string {
	return stsName + "-" + strconv.Itoa(ordinal)
}
