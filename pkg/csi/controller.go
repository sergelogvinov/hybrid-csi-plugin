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

package csi

import (
	"context"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientkubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_VOLUME,
	csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
}

// ControllerService is the controller service for the CSI driver
type ControllerService struct {
	Kclient clientkubernetes.Interface

	volumeLocks sync.Mutex

	csi.UnimplementedControllerServer
}

// NewControllerService returns a new controller service
func NewControllerService(kclient *clientkubernetes.Clientset) (*ControllerService, error) {
	return &ControllerService{
		Kclient: kclient,
	}, nil
}

// CreateVolume creates a volume
func (d *ControllerService) CreateVolume(_ context.Context, request *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).InfoS("CreateVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// DeleteVolume deletes a volume.
func (d *ControllerService) DeleteVolume(_ context.Context, request *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).InfoS("DeleteVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities get controller capabilities.
func (d *ControllerService) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).InfoS("ControllerGetCapabilities: called")

	caps := []*csi.ControllerServiceCapability{}

	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// ControllerPublishVolume publish a volume
func (d *ControllerService) ControllerPublishVolume(ctx context.Context, request *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).InfoS("ControllerPublishVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerUnpublishVolume unpublish a volume
func (d *ControllerService) ControllerUnpublishVolume(ctx context.Context, request *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).InfoS("ControllerUnpublishVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ValidateVolumeCapabilities validate volume capabilities
func (d *ControllerService) ValidateVolumeCapabilities(_ context.Context, request *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).InfoS("ValidateVolumeCapabilities: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ListVolumes list volumes
func (d *ControllerService) ListVolumes(_ context.Context, request *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).InfoS("ListVolumes: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// GetCapacity get capacity
func (d *ControllerService) GetCapacity(_ context.Context, request *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(5).InfoS("GetCapacity: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// CreateSnapshot create a snapshot
func (d *ControllerService) CreateSnapshot(_ context.Context, request *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.V(4).InfoS("CreateSnapshot: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// DeleteSnapshot delete a snapshot
func (d *ControllerService) DeleteSnapshot(_ context.Context, request *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.V(4).InfoS("DeleteSnapshot: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ListSnapshots list snapshots
func (d *ControllerService) ListSnapshots(_ context.Context, request *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	klog.V(4).InfoS("ListSnapshots: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume expand a volume
func (d *ControllerService) ControllerExpandVolume(_ context.Context, request *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	klog.V(4).InfoS("ControllerExpandVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetVolume get a volume
func (d *ControllerService) ControllerGetVolume(_ context.Context, request *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	klog.V(4).InfoS("ControllerGetVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerModifyVolume modify a volume
func (d *ControllerService) ControllerModifyVolume(_ context.Context, request *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	klog.V(4).InfoS("ControllerModifyVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}
