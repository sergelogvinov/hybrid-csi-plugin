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
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	corev1 "k8s.io/api/core/v1"
)

const (
	keyRegion = "topology.kubernetes.io/region"
	keyZone   = "topology.kubernetes.io/zone"
)

func TestFlatten(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []corev1.TopologySelectorTerm
		expected []topologyTerm
	}{
		{
			name:     "empty",
			allowed:  nil,
			expected: nil,
		},
		{
			name:    "single key, multiple values",
			allowed: []corev1.TopologySelectorTerm{testTopology(keyZone, "z1", "z2")},
			expected: []topologyTerm{
				{{keyZone, "z1"}},
				{{keyZone, "z2"}},
			},
		},
		{
			name: "AND of keys is distributed over OR of values and sorted",
			allowed: []corev1.TopologySelectorTerm{{
				MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{
					{Key: keyZone, Values: []string{"z1", "z2"}},
					{Key: keyRegion, Values: []string{"r1"}},
				},
			}},
			expected: []topologyTerm{
				{{keyRegion, "r1"}, {keyZone, "z1"}},
				{{keyRegion, "r1"}, {keyZone, "z2"}},
			},
		},
		{
			name: "OR of terms",
			allowed: []corev1.TopologySelectorTerm{
				testTopology(keyZone, "z1"),
				testTopology(keyRegion, "r2"),
			},
			expected: []topologyTerm{
				{{keyZone, "z1"}},
				{{keyRegion, "r2"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flatten(tt.allowed)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("flatten() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGetTopologyKeys(t *testing.T) {
	csiNode := testCSINode("node-1", "csi.a:"+keyRegion+","+keyZone, "csi.b")

	if got := getTopologyKeys(csiNode, "csi.a"); !reflect.DeepEqual(got, []string{keyRegion, keyZone}) {
		t.Errorf("getTopologyKeys(csi.a) = %v", got)
	}

	if got := getTopologyKeys(csiNode, "csi.b"); got != nil {
		t.Errorf("getTopologyKeys(csi.b) = %v, want nil", got)
	}

	if got := getTopologyKeys(csiNode, "csi.missing"); got != nil {
		t.Errorf("getTopologyKeys(csi.missing) = %v, want nil", got)
	}
}

func TestGetTopologyFromNode(t *testing.T) {
	node := testNode("node-1", map[string]string{keyZone: "z1", keyRegion: "r1"})

	term, missing := getTopologyFromNode(node, []string{keyZone, keyRegion})
	if missing {
		t.Fatalf("getTopologyFromNode() reported missing key")
	}

	if want := (topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}); !reflect.DeepEqual(term, want) {
		t.Errorf("getTopologyFromNode() = %v, want %v", term, want)
	}

	if _, missing = getTopologyFromNode(node, []string{keyZone, "missing"}); !missing {
		t.Errorf("getTopologyFromNode() did not report missing key")
	}

	term, missing = getTopologyFromNode(node, nil)
	if missing || len(term) != 0 {
		t.Errorf("getTopologyFromNode(nil) = %v, %v; want empty, false", term, missing)
	}
}

func TestTopologyTermSubset(t *testing.T) {
	node := topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}

	tests := []struct {
		name     string
		term     topologyTerm
		other    topologyTerm
		expected bool
	}{
		{"empty term is a subset of anything", topologyTerm{}, node, true},
		{"same", topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}, node, true},
		{"one key", topologyTerm{{keyZone, "z1"}}, node, true},
		{"different value", topologyTerm{{keyZone, "z2"}}, node, false},
		{"missing key", topologyTerm{{"other", "x"}}, node, false},
		{"other is empty", topologyTerm{{keyZone, "z1"}}, topologyTerm{}, false},
		{"term is longer", topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}, {"zz", "x"}}, node, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.term.subset(tt.other); got != tt.expected {
				t.Errorf("%v.subset(%v) = %v, want %v", tt.term, tt.other, got, tt.expected)
			}
		})
	}
}

func TestTopologyTermMatchLabels(t *testing.T) {
	labels := map[string]string{keyRegion: "r1", keyZone: "z1"}

	tests := []struct {
		name     string
		term     topologyTerm
		expected bool
	}{
		{"empty", topologyTerm{}, true},
		{"all match", topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}, true},
		{"one matches", topologyTerm{{keyZone, "z1"}}, true},
		{"different value", topologyTerm{{keyZone, "z2"}}, false},
		{"missing label", topologyTerm{{"other", "x"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.term.matchLabels(labels); got != tt.expected {
				t.Errorf("%v.matchLabels() = %v, want %v", tt.term, got, tt.expected)
			}
		})
	}
}

func TestTopologyTermSortCompare(t *testing.T) {
	a := topologyTerm{{keyZone, "z1"}, {keyRegion, "r1"}}
	a.sort()

	if want := (topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}); !reflect.DeepEqual(a, want) {
		t.Errorf("sort() = %v, want %v", a, want)
	}

	if c := a.compare(topologyTerm{{keyRegion, "r1"}, {keyZone, "z1"}}); c != 0 {
		t.Errorf("compare(equal) = %d, want 0", c)
	}

	if c := a.compare(topologyTerm{{keyRegion, "r1"}}); c <= 0 {
		t.Errorf("compare(shorter) = %d, want > 0", c)
	}

	if c := a.compare(topologyTerm{{keyRegion, "r1"}, {keyZone, "z2"}}); c >= 0 {
		t.Errorf("compare(bigger value) = %d, want < 0", c)
	}

	if c := a.compare(topologyTerm{{keyRegion, "r1"}, {"a", "z1"}}); c <= 0 {
		t.Errorf("compare(smaller key) = %d, want > 0", c)
	}
}

func TestBuildTopologyKeySelector(t *testing.T) {
	selector, err := buildTopologyKeySelector([]string{keyRegion, keyZone})
	if err != nil {
		t.Fatalf("buildTopologyKeySelector() error = %v", err)
	}

	if got, want := selector.String(), keyRegion+","+keyZone; got != want {
		t.Errorf("selector = %q, want %q", got, want)
	}

	if _, err = buildTopologyKeySelector([]string{"invalid key!"}); err == nil {
		t.Errorf("buildTopologyKeySelector(invalid) expected error")
	}
}

func TestToCSITopology(t *testing.T) {
	got := toCSITopology([]topologyTerm{
		{{keyRegion, "r1"}, {keyZone, "z1"}},
		{{keyZone, "z2"}},
	})

	want := []*csi.Topology{
		{Segments: map[string]string{keyRegion: "r1", keyZone: "z1"}},
		{Segments: map[string]string{keyZone: "z2"}},
	}

	if len(got) != len(want) {
		t.Fatalf("toCSITopology() len = %d, want %d", len(got), len(want))
	}

	for i := range want {
		if !reflect.DeepEqual(got[i].GetSegments(), want[i].GetSegments()) {
			t.Errorf("toCSITopology()[%d] = %v, want %v", i, got[i].GetSegments(), want[i].GetSegments())
		}
	}
}
