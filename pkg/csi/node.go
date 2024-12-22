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
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var nodeCaps = []csi.NodeServiceCapability_RPC_Type{
	csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
}

var volumeCaps = []csi.VolumeCapability_AccessMode{
	{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	},
	{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
	},
	{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	},
	{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	},
}

// NodeService is the node service for the CSI driver
type NodeService struct {
	nodeID  string
	kclient kubernetes.Interface
	csi.UnimplementedNodeServer
}

// NewNodeService returns a new NodeService
func NewNodeService(nodeID string, clientSet kubernetes.Interface) *NodeService {
	return &NodeService{
		nodeID:  nodeID,
		kclient: clientSet,
	}
}

// NodeStageVolume is called by the CO when a workload that wants to use the specified volume is placed (scheduled) on a node.
func (n *NodeService) NodeStageVolume(_ context.Context, request *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.V(4).InfoS("NodeStageVolume: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume is called by the CO when a workload that was using the specified volume is being moved to a different node.
//
//nolint:dupl
func (n *NodeService) NodeUnstageVolume(_ context.Context, request *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(4).InfoS("NodeUnstageVolume: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume mounts the volume on the node.
func (n *NodeService) NodePublishVolume(_ context.Context, request *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).InfoS("NodePublishVolume: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmount the volume from the target path
func (n *NodeService) NodeUnpublishVolume(_ context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).InfoS("NodeUnpublishVolume: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats get the volume stats
func (n *NodeService) NodeGetVolumeStats(_ context.Context, request *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.V(4).InfoS("NodeGetVolumeStats: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodeGetVolumeStatsResponse{}, nil
}

// NodeExpandVolume expand the volume
func (n *NodeService) NodeExpandVolume(_ context.Context, request *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	klog.V(4).InfoS("NodeExpandVolume: called", "args", protosanitizer.StripSecrets(request))

	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeGetCapabilities get the node capabilities
func (n *NodeService) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(4).InfoS("NodeGetCapabilities: called")

	caps := []*csi.NodeServiceCapability{}

	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeGetInfo get the node info
func (n *NodeService) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).InfoS("NodeGetInfo: called")

	node, err := n.kclient.CoreV1().Nodes().Get(ctx, n.nodeID, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", n.nodeID, err)
	}

	region := node.Labels[corev1.LabelTopologyRegion]
	if region == "" {
		return nil, fmt.Errorf("failed to get region for node %s", n.nodeID)
	}

	zone := node.Labels[corev1.LabelTopologyZone]
	if zone == "" {
		return nil, fmt.Errorf("failed to get zone for node %s", n.nodeID)
	}

	return &csi.NodeGetInfoResponse{
		NodeId:            n.nodeID,
		MaxVolumesPerNode: MaxVolumesPerNode,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				corev1.LabelTopologyRegion: region,
				corev1.LabelTopologyZone:   zone,
			},
		},
	}, nil
}
