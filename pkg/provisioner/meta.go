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

const (
	// DriverName is the name of the CSI driver
	DriverName = "csi.hybrid.sinextra.dev"
	// DriverVersion is the version of the CSI driver
	DriverVersion = "0.1.0"

	// metaPrefix is the domain prefix of all hybrid finalizers, labels and annotations.
	metaPrefix = DriverName + "/"

	// FinalizerProvisioning is set on the user PVC before the helper PVC is created.
	FinalizerProvisioning = metaPrefix + "provisioning"
	// FinalizerHelper is set on the helper PVC when it is created.
	FinalizerHelper = metaPrefix + "helper"

	// AnnotationBackendClass is the pinned backend StorageClass.
	AnnotationBackendClass = metaPrefix + "backend-class"
	// AnnotationHelper is the name of the helper PVC.
	AnnotationHelper = metaPrefix + "helper"
	// AnnotationReschedules is how many times the claim was rescheduled because of the helper timeout
	// or a volume that does not fit the selected node.
	AnnotationReschedules = metaPrefix + "reschedules"
	// AnnotationExpandable is set to "false" when the backend cannot expand volumes.
	AnnotationExpandable = metaPrefix + "expandable"
	// AnnotationOwnerUID is the UID of the user PVC that owns the helper PVC.
	AnnotationOwnerUID = metaPrefix + "owner-uid"

	// AnnotationClaim is the namespace/name of the user PVC.
	AnnotationClaim = metaPrefix + "claim"
	// AnnotationStorageClass is the hybrid StorageClass name.
	AnnotationStorageClass = metaPrefix + "storage-class"
	// AnnotationMigrated marks a PV provisioned by v0.x and adopted by the migration, value "true".
	AnnotationMigrated = metaPrefix + "migrated"
	// AnnotationReclaimPolicyChecked marks a migrated PV whose reclaim policy matched the hybrid
	// StorageClass or was fixed, value "true". It is not checked again: from then on the policy
	// may be changed on purpose.
	AnnotationReclaimPolicyChecked = metaPrefix + "reclaim-policy-checked"

	// LabelRole marks helper PVCs, value LabelRoleHelper.
	LabelRole = metaPrefix + "role"
	// LabelRoleHelper is the value of LabelRole on helper PVCs.
	LabelRoleHelper = "helper"
	// LabelManaged marks hybrid-managed PVs, value "true".
	LabelManaged = metaPrefix + "managed"
)

// Values of hybrid and Kubernetes annotations.
const (
	// ValueTrue is the value of the boolean labels and annotations.
	ValueTrue = "true"
	// valueYes is the value of the PV controller binding annotations (bind-completed, bound-by-controller).
	valueYes = "yes"
)

// Well-known Kubernetes annotations.
const (
	annotationBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	annotationStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	annotationSelectedNode           = "volume.kubernetes.io/selected-node"

	finalizerPVCProtection = "kubernetes.io/pvc-protection"

	// KindPersistentVolumeClaim is the kind of PVC references.
	KindPersistentVolumeClaim = "PersistentVolumeClaim"
)
