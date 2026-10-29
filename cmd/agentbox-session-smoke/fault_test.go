package main

import "testing"

func TestReplacementBoundaryRequiresSameLogicalResourcesAndNewPod(t *testing.T) {
	old := identity{Claim: "a", ClaimUID: "c", Sandbox: "s", SandboxUID: "su", Pod: "p", PodUID: "pu", Service: "service", ServiceUID: "sv", ServiceFQDN: "service.ns.svc"}
	next := old
	next.PodUID = "new-pod"
	next.PodIP = "10.244.1.20"
	if err := replacementBoundary(old, next); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*identity){func(v *identity) { v.PodUID = old.PodUID }, func(v *identity) { v.ClaimUID = "other" },
		func(v *identity) { v.SandboxUID = "other" }, func(v *identity) { v.ServiceUID = "other" }, func(v *identity) { v.ServiceFQDN = "other.ns.svc" }} {
		bad := next
		mutate(&bad)
		if err := replacementBoundary(old, bad); err == nil {
			t.Fatalf("invalid replacement accepted: %#v", bad)
		}
	}
}
func TestReplacementBoundaryRejectsIncompleteIdentity(t *testing.T) {
	if err := replacementBoundary(identity{}, identity{PodUID: "new"}); err == nil {
		t.Fatal("incomplete identity accepted")
	}
}
