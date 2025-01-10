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

package provisioner // copy from external-provisioner/pkg/controller/topology.go

import (
	"fmt"
	"slices"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type topologySegment struct {
	Key, Value string
}

// topologyTerm represents a single term where its topology key value pairs are AND'd together.
//
// Be sure to sort after construction for compare() and subset() to work properly.
type topologyTerm []topologySegment

func flatten(allowedTopologies []v1.TopologySelectorTerm) []topologyTerm {
	var finalTerms []topologyTerm
	for _, selectorTerm := range allowedTopologies { // OR

		var oldTerms []topologyTerm
		for _, selectorExpression := range selectorTerm.MatchLabelExpressions { // AND

			var newTerms []topologyTerm
			for _, v := range selectorExpression.Values { // OR
				// Distribute the OR over AND.

				if len(oldTerms) == 0 {
					// No previous terms to distribute over. Simply append the new term.
					newTerms = append(newTerms, topologyTerm{{selectorExpression.Key, v}})
				} else {
					for _, oldTerm := range oldTerms {
						// "Distribute" by adding an entry to the term
						newTerm := slices.Clone(oldTerm)
						newTerm = append(newTerm, topologySegment{selectorExpression.Key, v})
						newTerms = append(newTerms, newTerm)
					}
				}
			}

			oldTerms = newTerms
		}

		// Concatenate all OR'd terms.
		finalTerms = append(finalTerms, oldTerms...)
	}

	for _, term := range finalTerms {
		term.sort()
	}
	return finalTerms
}

func getTopologyKeys(csiNode *storagev1.CSINode, driverName string) []string {
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == driverName {
			return driver.TopologyKeys
		}
	}
	return nil
}

func getTopologyFromNode(node *v1.Node, topologyKeys []string) (term topologyTerm, isMissingKey bool) {
	term = make(topologyTerm, 0, len(topologyKeys))
	for _, key := range topologyKeys {
		v, ok := node.Labels[key]
		if !ok {
			return nil, true
		}
		term = append(term, topologySegment{key, v})
	}
	term.sort()
	return term, false
}

func buildTopologyKeySelector(topologyKeys []string) (labels.Selector, error) {
	var expr []metav1.LabelSelectorRequirement
	for _, key := range topologyKeys {
		expr = append(expr, metav1.LabelSelectorRequirement{
			Key:      key,
			Operator: metav1.LabelSelectorOpExists,
		})
	}

	labelSelector := metav1.LabelSelector{
		MatchExpressions: expr,
	}

	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, fmt.Errorf("error parsing topology keys selector: %v", err)
	}

	return selector, nil
}

func (t topologyTerm) sort() {
	slices.SortFunc(t, func(a, b topologySegment) int {
		r := strings.Compare(a.Key, b.Key)
		if r != 0 {
			return r
		}
		// Should not happen currently. We may support multi-value in the future?
		return strings.Compare(a.Value, b.Value)
	})
}

func (t topologyTerm) compare(other topologyTerm) int {
	if len(t) != len(other) {
		return len(t) - len(other)
	}
	for i, k1 := range t {
		k2 := other[i]
		r := strings.Compare(k1.Key, k2.Key)
		if r != 0 {
			return r
		}
		r = strings.Compare(k1.Value, k2.Value)
		if r != 0 {
			return r
		}
	}
	return 0
}

func (t topologyTerm) subset(other topologyTerm) bool {
	if len(t) == 0 {
		return true
	}
	j := 0
	for _, k2 := range other {
		k1 := t[j]
		if k1.Key != k2.Key {
			continue
		}
		if k1.Value != k2.Value {
			return false
		}
		j++
		if j == len(t) {
			// All segments in t have been checked and is present in other.
			return true
		}
	}
	return false
}

func toCSITopology(terms []topologyTerm) []*csi.Topology {
	out := make([]*csi.Topology, 0, len(terms))
	for _, term := range terms {
		segs := make(map[string]string, len(term))
		for _, k := range term {
			segs[k.Key] = k.Value
		}
		out = append(out, &csi.Topology{Segments: segs})
	}
	return out
}
