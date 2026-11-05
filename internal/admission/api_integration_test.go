//go:build integration

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"testing"
	"time"
)

// The platform harness supplies its owned disposable namespace. No default
// kubeconfig or shared production namespace is ever selected implicitly.
func TestRealStatusCASPreservesOtherChecksAndRefusesReplacement(t *testing.T) {
	path, namespace, owner := os.Getenv("AGENTBOX_ADMISSION_TEST_KUBECONFIG"), os.Getenv("AGENTBOX_ADMISSION_TEST_NAMESPACE"), os.Getenv("AGENTBOX_ADMISSION_TEST_OWNER")
	if path == "" || namespace == "" || owner == "" {
		t.Skip("explicit owned platform test scope required")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal("test credentials unavailable")
	}
	cfg.Timeout = 5 * time.Second
	cl, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns, err := cl.Resource(Namespaces).Get(ctx, namespace, metav1.GetOptions{})
	if err != nil || ns.GetLabels()["agentbox.dev/session"] != owner {
		t.Fatal("foreign API test namespace")
	}
	o := workload(fmt.Sprintf("admission-cas-%d", time.Now().UnixNano()))
	o.SetNamespace(namespace)
	o.SetUID("")
	o.SetResourceVersion("")
	o.SetCreationTimestamp(metav1.Time{})
	o.SetGeneration(0)
	delete(o.Object, "status")
	// No Job owner and no real queue: this object can never start computation.
	_ = unstructured.SetNestedField(o.Object, "missing-api-test-queue", "spec", "queueName")
	created, err := cl.Resource(Workloads).Namespace(namespace).Create(ctx, o, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		uid := created.GetUID()
		_ = cl.Resource(Workloads).Namespace(namespace).Delete(context.Background(), created.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	}()
	var ready *unstructured.Unstructured
	for attempt := 0; attempt < 5; attempt++ {
		current, e := cl.Resource(Workloads).Namespace(namespace).Get(ctx, created.GetName(), metav1.GetOptions{})
		if e != nil {
			t.Fatal(e)
		}
		status := workload("shape").Object["status"]
		admission := status.(map[string]any)["admission"].(map[string]any)
		admission["podSetAssignments"] = []any{map[string]any{"name": "main", "flavors": map[string]any{"cpu": "test", "memory": "test"}, "resourceUsage": map[string]any{"cpu": "500m", "memory": "256Mi"}, "count": int64(1)}}
		operations := fences(current)
		operations = append(operations, map[string]any{"op": "add", "path": "/status", "value": status})
		data, _ := json.Marshal(operations)
		ready, e = cl.Resource(Workloads).Namespace(namespace).Patch(ctx, current.GetName(), types.JSONPatchType, data, metav1.PatchOptions{}, "status")
		if e == nil {
			break
		}
		if !apierrors.IsConflict(e) && !apierrors.IsInvalid(e) {
			t.Fatal(e)
		}
	}
	if ready == nil {
		t.Fatal("could not initialize real status fixture")
	}
	// A concurrent status writer changes Admitted and the resourceVersion.
	competed := false
	for attempt := 0; attempt < 5; attempt++ {
		fresh, e := cl.Resource(Workloads).Namespace(namespace).Get(ctx, ready.GetName(), metav1.GetOptions{})
		if e != nil {
			t.Fatal(e)
		}
		conditions, _, _ := unstructured.NestedSlice(fresh.Object, "status", "conditions")
		filtered := []any{}
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if ok && condition["type"] != "Admitted" {
				filtered = append(filtered, raw)
			}
		}
		filtered = append(filtered, map[string]any{"type": "Admitted", "status": "True", "reason": "APIConcurrencyTest", "message": "test", "observedGeneration": fresh.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)})
		operations := fences(fresh)
		operations = append(operations, map[string]any{"op": "add", "path": "/status/conditions", "value": filtered})
		data, _ := json.Marshal(operations)
		_, e = cl.Resource(Workloads).Namespace(namespace).Patch(ctx, ready.GetName(), types.JSONPatchType, data, metav1.PatchOptions{}, "status")
		if e == nil {
			competed = true
			break
		}
		if !apierrors.IsConflict(e) && !apierrors.IsInvalid(e) {
			t.Fatal(e)
		}
	}
	if !competed {
		t.Fatal("test competitor could not commit within its bounded CAS attempts")
	}
	controller := &Controller{client: cl, cfg: policy()}
	controller.cfg.Namespace = namespace
	if err = controller.writeCheck(ctx, ready, "Retry", "check_wait_exceeded", time.Now()); !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
		t.Fatal("real API did not reject stale Retry", err)
	}
	latest, err := cl.Resource(Workloads).Namespace(namespace).Get(ctx, ready.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, other, ok := checkEntry(latest, "other")
	if !ok || other["message"] != "owned elsewhere" {
		t.Fatal("unrelated check changed")
	}
	if eligible(latest) {
		t.Fatal("fresh admitted object remained eligible")
	}
	// Replacing the UID must invalidate an old mutation even with the current RV.
	old := ready.DeepCopy()
	old.SetResourceVersion(latest.GetResourceVersion())
	old.SetUID("a-different-uid")
	if err = controller.writeCheck(ctx, old, "Ready", "test", time.Now()); !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
		t.Fatal("real API did not reject stale UID", err)
	}
}
