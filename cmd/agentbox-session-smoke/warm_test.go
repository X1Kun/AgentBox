package main

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestWarmAllocationRequiresOriginalSandboxAndPodUIDs(t *testing.T) {
	supply := []supplyIdentity{{Sandbox: "s-a", SandboxUID: "sa", Pod: "p-a", PodUID: "pa"}, {Sandbox: "s-b", SandboxUID: "sb", Pod: "p-b", PodUID: "pb"}}
	ids := []identity{{ClaimUID: "ca", Sandbox: "s-a", SandboxUID: "sa", Pod: "p-a", PodUID: "pa"}, {ClaimUID: "cb", Sandbox: "s-b", SandboxUID: "sb", Pod: "p-b", PodUID: "pb"}}
	if err := allocatedFrom(ids, supply); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func([]identity){
		func(v []identity) { v[0].SandboxUID = "replaced" }, func(v []identity) { v[0].PodUID = "cold-created" },
		func(v []identity) { v[1] = v[0] }, func(v []identity) { v[1].ClaimUID = v[0].ClaimUID }, func(v []identity) { v[0].ClaimUID = "" },
	} {
		bad := append([]identity(nil), ids...)
		change(bad)
		if err := allocatedFrom(bad, supply); err == nil {
			t.Fatalf("invalid allocation accepted: %#v", bad)
		}
	}
}
func TestRecoveredInventoryRejectsAnyUsedIdentity(t *testing.T) {
	used := []identity{{SandboxUID: "used-s", PodUID: "used-p"}}
	for _, s := range []supplyIdentity{{SandboxUID: "used-s", PodUID: "new-p"}, {SandboxUID: "new-s", PodUID: "used-p"}} {
		if err := freshSupply([]supplyIdentity{s}, used); err == nil {
			t.Fatalf("used environment accepted: %#v", s)
		}
	}
	if err := freshSupply([]supplyIdentity{{SandboxUID: "new-s", PodUID: "new-p"}}, used); err != nil {
		t.Fatal(err)
	}
}
func TestWarmReadinessRequiresCurrentGeneration(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"generation": int64(2)}, "status": map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)},
	}}}}
	if readySandbox(obj) {
		t.Fatal("stale readiness accepted")
	}
	_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}, "status", "conditions")
	if !readySandbox(obj) {
		t.Fatal("current readiness rejected")
	}
}
