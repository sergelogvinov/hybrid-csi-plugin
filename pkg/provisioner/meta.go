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

// Metadata contract, see docs/design.md §4.
//
// U - user PVC, H - helper PVC, P - backend PV.

const (
	// DriverName is the name of the CSI driver
	DriverName = "csi.hybrid.sinextra.dev"
	// DriverVersion is the version of the CSI driver
	DriverVersion = "0.1.0"

	// metaPrefix is the domain prefix of all hybrid finalizers, labels and annotations.
	metaPrefix = DriverName + "/"
)

// Finalizers (§4.1).
const (
	// FinalizerProvisioning is set on U before H is created.
	FinalizerProvisioning = metaPrefix + "provisioning"
	// FinalizerHelper is set on H when it is created.
	FinalizerHelper = metaPrefix + "helper"
)

// Annotations and labels on U (§4.2).
const (
	// AnnBackendClass is the pinned backend StorageClass.
	AnnBackendClass = metaPrefix + "backend-class"
	// AnnHelper is the name of H.
	AnnHelper = metaPrefix + "helper"
	// AnnReschedules is how many times the helper timeout has caused a reschedule.
	AnnReschedules = metaPrefix + "reschedules"
	// AnnExpandable is set to "false" when the backend cannot expand volumes.
	AnnExpandable = metaPrefix + "expandable"
)

// Annotations and labels on H (§4.2).
const (
	// LabelRole marks helper PVCs, value LabelRoleHelper.
	LabelRole = metaPrefix + "role"
	// LabelRoleHelper is the value of LabelRole on H.
	LabelRoleHelper = "helper"
	// AnnOwnerUID is the UID of U that owns H.
	AnnOwnerUID = metaPrefix + "owner-uid"
)

// Annotations and labels on P (§4.2).
const (
	// LabelManaged marks hybrid-managed PVs, value "true".
	LabelManaged = metaPrefix + "managed"
	// AnnClaim is the namespace/name of U.
	AnnClaim = metaPrefix + "claim"
	// AnnStorageClass is the hybrid StorageClass name.
	AnnStorageClass = metaPrefix + "storage-class"
)

// Well-known Kubernetes annotations.
const (
	annBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	annStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	annSelectedNode           = "volume.kubernetes.io/selected-node"

	finalizerPVCProtection = "kubernetes.io/pvc-protection"
)

// StorageClass parameters.
const (
	// paramStorageClasses is the comma-separated list of backend StorageClasses.
	paramStorageClasses = "storageClasses"
)

// Provisioning methods.
const (
	methodDefault    = "auto"
	methodPod        = "pod"
	methodAnnotation = "annotation"
)
