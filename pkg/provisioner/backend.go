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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/klog/v2"
)

// backendClasses returns the ordered list of backend StorageClass names of a hybrid StorageClass.
func backendClasses(hsc *storagev1.StorageClass) ([]string, error) {
	classes, ok := hsc.Parameters[paramStorageClasses]
	if !ok {
		return nil, fmt.Errorf("%s parameter is required", paramStorageClasses)
	}

	return strings.Split(classes, ","), nil
}

// selectBackend returns the first backend StorageClass from the list that can serve the node.
func (p *HybridProvisioner) selectBackend(node *corev1.Node, storageClasses []string) (*storagev1.StorageClass, error) {
	csiNode, err := p.csiNodeLister.Get(node.Name)
	if err != nil {
		return nil, fmt.Errorf("error getting CSINode for selected node %q: %v", node.Name, err)
	}

	for _, name := range storageClasses {
		class, err := p.scLister.Get(name)
		if err != nil {
			klog.V(4).InfoS("storage class is not found", "node", klog.KObj(node), "storageClass", name)

			continue
		}

		if err := p.fits(class, node, csiNode); err != nil {
			klog.V(4).InfoS("storage class does not fit the node", "node", klog.KObj(node), "storageClass", name, "reason", err)

			continue
		}

		return class, nil
	}

	return nil, fmt.Errorf("no matching storage class found for selected node %q", node.Name)
}

// fits checks whether the backend StorageClass can provision a volume on the node.
// It returns nil if it can, or an error with the reason.
//
// This is the single predicate for backend selection (docs/design.md §5.3 step 2 a–c).
func (p *HybridProvisioner) fits(class *storagev1.StorageClass, node *corev1.Node, csiNode *storagev1.CSINode) error {
	if len(class.AllowedTopologies) > 0 {
		if err := allowedTopologiesFit(class, node, csiNode); err != nil {
			return err
		}
	}

	if driver, err := p.driverLister.Get(class.Provisioner); err != nil || driver == nil {
		// Provisioner is not a CSI driver
		return nil // nolint: nilerr
	}

	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == class.Provisioner {
			return nil
		}
	}

	return fmt.Errorf("driver %q is not registered on node %q", class.Provisioner, node.Name)
}

// allowedTopologiesFit checks the StorageClass allowedTopologies against the node.
//
// For CSI drivers the node topology is built from the driver topology keys registered in CSINode.
// For drivers without topology keys (non-CSI provisioners), the allowed terms are matched
// directly against the node labels, as the scheduler does.
func allowedTopologiesFit(class *storagev1.StorageClass, node *corev1.Node, csiNode *storagev1.CSINode) error {
	allowedTopologies := flatten(class.AllowedTopologies)

	topologyKeys := getTopologyKeys(csiNode, class.Provisioner)
	if len(topologyKeys) == 0 {
		for _, t := range allowedTopologies {
			if t.matchLabels(node.Labels) {
				return nil
			}
		}

		return fmt.Errorf("node labels are not in allowed topologies")
	}

	selectedTopology, isMissingKey := getTopologyFromNode(node, topologyKeys)
	if isMissingKey {
		return fmt.Errorf("node is missing topology keys %v", topologyKeys)
	}

	for _, t := range allowedTopologies {
		if t.subset(selectedTopology) {
			return nil
		}
	}

	return fmt.Errorf("topology %v is not in allowed topologies", selectedTopology)
}
