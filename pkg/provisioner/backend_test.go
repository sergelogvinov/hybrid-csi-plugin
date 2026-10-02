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

	"k8s.io/apimachinery/pkg/runtime"
)

const (
	csiA     = "csi.a.example.com"
	csiB     = "csi.b.example.com"
	nonCSI   = "rancher.io/local-path"
	labelBkd = "example.com/backend"

	localClass = "local"
)

func TestBackendClasses(t *testing.T) {
	got, err := backendClasses(testHybridStorageClass(testHybridClass, "a", "b", "c"))
	if err != nil {
		t.Fatalf("backendClasses() error = %v", err)
	}

	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("backendClasses() = %v, want %v", got, want)
	}

	if _, err = backendClasses(testStorageClass(testHybridClass, DriverName)); err == nil {
		t.Errorf("backendClasses() without parameter expected error")
	}
}

func TestSelectBackend(t *testing.T) {
	tests := []struct {
		name     string
		objs     []runtime.Object
		node     string
		classes  []string
		expected string
		wantErr  bool
	}{
		{
			name: "first class wins",
			objs: []runtime.Object{
				testCSIDriver(csiA), testCSIDriver(csiB),
				testCSINode("node-1", csiA, csiB),
				testStorageClass("a", csiA), testStorageClass("b", csiB),
			},
			classes:  []string{"a", "b"},
			expected: "a",
		},
		{
			name: "missing class is skipped",
			objs: []runtime.Object{
				testCSIDriver(csiB),
				testCSINode("node-1", csiB),
				testStorageClass("b", csiB),
			},
			classes:  []string{"missing", "b"},
			expected: "b",
		},
		{
			name: "CSI driver not registered on the node is skipped",
			objs: []runtime.Object{
				testCSIDriver(csiA), testCSIDriver(csiB),
				testCSINode("node-1", csiB),
				testStorageClass("a", csiA), testStorageClass("b", csiB),
			},
			classes:  []string{"a", "b"},
			expected: "b",
		},
		{
			name: "non-CSI provisioner is accepted on any node",
			objs: []runtime.Object{
				testCSINode("node-1"),
				testStorageClass(localClass, nonCSI),
			},
			classes:  []string{localClass},
			expected: localClass,
		},
		{
			name: "CSI allowedTopologies match driver topology keys",
			objs: []runtime.Object{
				testCSIDriver(csiA), testCSIDriver(csiB),
				testCSINode("node-1", csiA+":"+keyZone, csiB+":"+keyZone),
				testStorageClass("a", csiA, testTopology(keyZone, "z2")),
				testStorageClass("b", csiB, testTopology(keyZone, "z0", "z1")),
			},
			classes:  []string{"a", "b"},
			expected: "b",
		},
		{
			name: "CSI allowedTopologies with node missing a topology key",
			objs: []runtime.Object{
				testCSIDriver(csiA),
				testCSINode("node-1", csiA+":"+keyZone+",missing"),
				testStorageClass("a", csiA, testTopology(keyZone, "z1")),
			},
			classes: []string{"a"},
			wantErr: true,
		},
		{
			name: "non-CSI allowedTopologies match node labels",
			objs: []runtime.Object{
				testCSINode("node-1"),
				testStorageClass("local-a", nonCSI, testTopology(labelBkd, "a")),
				testStorageClass("local-b", nonCSI, testTopology(labelBkd, "b")),
			},
			classes:  []string{"local-a", "local-b"},
			expected: "local-b",
		},
		{
			name: "non-CSI allowedTopologies do not match",
			objs: []runtime.Object{
				testCSINode("node-1"),
				testStorageClass("local-a", nonCSI, testTopology(labelBkd, "a")),
			},
			classes: []string{"local-a"},
			wantErr: true,
		},
		{
			name: "no CSINode",
			objs: []runtime.Object{
				testStorageClass(localClass, nonCSI),
			},
			classes: []string{localClass},
			wantErr: true,
		},
		{
			name: "nothing fits",
			objs: []runtime.Object{
				testCSIDriver(csiA),
				testCSINode("node-1"),
				testStorageClass("a", csiA),
			},
			classes: []string{"a"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode("node-1", map[string]string{keyZone: "z1", labelBkd: "b"})
			env := newTestEnv(t, methodDefault, append(tt.objs, node)...)

			class, err := env.prov.selectBackend(node, tt.classes)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("selectBackend() = %s, want error", class.Name)
				}

				return
			}

			if err != nil {
				t.Fatalf("selectBackend() error = %v", err)
			}

			if class.Name != tt.expected {
				t.Errorf("selectBackend() = %s, want %s", class.Name, tt.expected)
			}
		})
	}
}
