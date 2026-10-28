package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/X1Kun/AgentBox/internal/sessionprobe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

type supplyIdentity struct {
	Sandbox    string `json:"sandbox"`
	SandboxUID string `json:"sandbox_uid"`
	Pod        string `json:"pod"`
	PodUID     string `json:"pod_uid"`
}
type warmReport struct {
	PoolUID         string           `json:"pool_uid"`
	QuotaUID        string           `json:"quota_uid"`
	Initial         []supplyIdentity `json:"initial"`
	Recovered       []supplyIdentity `json:"recovered"`
	ExhaustionError string           `json:"exhaustion_error,omitempty"`
}

func eventually(ctx context.Context, budget time.Duration, check func() (bool, error)) error {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("warm-pool behavior did not converge before deadline")
		case <-ticker.C:
		}
	}
}
func readySandbox(obj *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if condition["type"] == "Ready" && condition["status"] == "True" && condition["observedGeneration"] == obj.GetGeneration() {
			return true
		}
	}
	return false
}

// Inspect actual ownership and Pods, rather than trusting a replica count alone.
func (p *probe) supply(ctx context.Context, expected int) ([]supplyIdentity, error) {
	var snapshot []supplyIdentity
	err := eventually(ctx, 180*time.Second, func() (bool, error) {
		pool, err := p.dynamic.Resource(pools).Namespace(p.namespace).Get(ctx, p.pool, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		if !controlled(pool, p.owner, p.run) || replicas != 2 || string(pool.GetUID()) != p.result.Warm.PoolUID {
			return false, errors.New("warm pool identity or fixed target changed")
		}
		observed, _, _ := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
		ready, _, _ := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
		if observed != pool.GetGeneration() || ready != int64(expected) {
			return false, nil
		}
		objects, err := p.dynamic.Resource(sandboxes).Namespace(p.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		pods, err := p.core.Pods(p.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 3 {
			return false, errors.New("namespace exceeded its three-Pod budget")
		}
		snapshot = nil
		for i := range objects.Items {
			obj := &objects.Items[i]
			if !ownedBy(obj.GetOwnerReferences(), p.result.Warm.PoolUID) || !obj.GetDeletionTimestamp().IsZero() || !readySandbox(obj) {
				continue
			}
			for _, pod := range pods.Items {
				if !ownedBy(pod.OwnerReferences, string(obj.GetUID())) {
					continue
				}
				if !controlled(&pod, p.owner, p.run) || pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "agentbox-gvisor" || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
					return false, errors.New("unexpected warm Pod boundary")
				}
				for _, c := range pod.Status.Conditions {
					if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
						snapshot = append(snapshot, supplyIdentity{obj.GetName(), string(obj.GetUID()), pod.Name, string(pod.UID)})
					}
				}
			}
		}
		return len(snapshot) == expected, nil
	})
	return snapshot, err
}

func allocatedFrom(ids []identity, supply []supplyIdentity) error {
	seenPods, seenClaims := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		if id.ClaimUID == "" || id.PodUID == "" || seenPods[id.PodUID] || seenClaims[id.ClaimUID] {
			return errors.New("duplicate or incomplete warm allocation")
		}
		seenPods[id.PodUID], seenClaims[id.ClaimUID] = true, true
		if !slices.ContainsFunc(supply, func(s supplyIdentity) bool {
			return s.Sandbox == id.Sandbox && s.SandboxUID == id.SandboxUID && s.Pod == id.Pod && s.PodUID == id.PodUID
		}) {
			return errors.New("claim did not retain a pre-existing warm Sandbox and Pod")
		}
	}
	return nil
}
func freshSupply(supply []supplyIdentity, used []identity) error {
	for _, s := range supply {
		for _, id := range used {
			if s.PodUID == id.PodUID || s.SandboxUID == id.SandboxUID {
				return errors.New("used environment returned to warm inventory")
			}
		}
	}
	return nil
}
func (p *probe) warmCheck(name string) {
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: name, Passed: true})
}

func (p *probe) runWarm(ctx context.Context, config *rest.Config) (result report, retErr error) {
	p.pool = "python-warm"
	p.result.Warm = &warmReport{}
	defer func() { result = p.result }()
	pool, err := p.dynamic.Resource(pools).Namespace(p.namespace).Get(ctx, p.pool, metav1.GetOptions{})
	if err != nil {
		return result, err
	}
	if !controlled(pool, p.owner, p.run) {
		return result, errors.New("unowned warm pool")
	}
	p.result.Warm.PoolUID = string(pool.GetUID())
	quota, err := p.core.ResourceQuotas(p.namespace).Get(ctx, "session-budget", metav1.GetOptions{})
	if err != nil {
		return result, err
	}
	if !controlled(quota, p.owner, p.run) || quota.Spec.Hard.Pods().Value() != 3 || quota.Status.Hard.Pods().Value() != 3 {
		return result, errors.New("three-Pod ResourceQuota is not active")
	}
	p.result.Warm.QuotaUID = string(quota.UID)
	initial, err := p.supply(ctx, 2)
	if err != nil {
		return result, err
	}
	p.result.Warm.Initial = initial
	p.warmCheck("fixed_two_ready_members_before_claims")
	p.emit("warm-prepared")
	if err := waitMarker(ctx, "warm-start"); err != nil {
		return result, err
	}
	client, err := sdk.NewClient(ctx, sdk.Options{Namespace: p.namespace, Runtime: sdk.RuntimeSandboxd, Connectivity: sdk.ConnectivityInClusterService,
		RestConfig: config, Labels: map[string]string{ownerLabel: p.owner, runLabel: p.run}, Quiet: true, ShutdownAfter: 10 * time.Minute,
		SandboxReadyTimeout: 180 * time.Second, CleanupTimeout: 60 * time.Second, RequestTimeout: 15 * time.Second, PerAttemptTimeout: 10 * time.Second, MaxUploadSize: 1 << 20, MaxDownloadSize: 1 << 20})
	if err != nil {
		return result, err
	}
	var handles []*sdk.Sandbox
	defer func() {
		for _, s := range handles {
			cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			e := s.Close(cleanup)
			cancel()
			retErr = errors.Join(retErr, e)
		}
	}()
	// Issue each creation exactly once, concurrently. No application-level replay.
	var pair [2]*sdk.Sandbox
	var failures [2]error
	var wg sync.WaitGroup
	for i := range pair {
		wg.Go(func() { pair[i], failures[i] = client.CreateSandbox(ctx, p.pool, p.namespace) })
	}
	wg.Wait()
	for _, s := range pair {
		if s != nil {
			handles = append(handles, s)
		}
	}
	for _, e := range failures {
		if e != nil {
			return result, e
		}
	}
	for i, s := range pair {
		id, e := p.identify(ctx, s)
		if e != nil {
			return result, e
		}
		p.result.Sessions = append(p.result.Sessions, id)
		marker := fmt.Sprintf("private-%d", i)
		if e := s.Write(ctx, marker, []byte("used")); e != nil {
			return result, e
		}
	}
	if err := allocatedFrom(p.result.Sessions, initial); err != nil {
		return result, err
	}
	p.warmCheck("concurrent_claims_keep_distinct_precreated_pods")
	if exists, err := pair[1].Exists(ctx, "private-0"); err != nil || exists {
		return result, errors.New("parallel warm sessions share files")
	}
	p.warmCheck("parallel_warm_sessions_keep_files_isolated")
	replacement, err := p.supply(ctx, 1)
	if err != nil {
		return result, err
	}
	if err := freshSupply(replacement, p.result.Sessions); err != nil {
		return result, err
	}
	p.warmCheck("pool_replenishes_within_total_pod_budget")
	third, err := client.CreateSandbox(ctx, p.pool, p.namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, third)
	thirdID, err := p.identify(ctx, third)
	if err != nil {
		return result, err
	}
	if err := allocatedFrom([]identity{thirdID}, replacement); err != nil {
		return result, err
	}
	p.result.Sessions = append(p.result.Sessions, thirdID)
	if err := third.Write(ctx, "private-2", []byte("used")); err != nil {
		return result, err
	}
	if _, err := p.supply(ctx, 0); err != nil {
		return result, err
	}
	p.emit("warm-claimed")
	if err := waitMarker(ctx, "warm-exhaust"); err != nil {
		return result, err
	}
	extra, err := client.CreateSandbox(ctx, p.pool, p.namespace, sdk.WithSandboxReadyTimeout(20*time.Second))
	if extra != nil {
		handles = append(handles, extra)
		return result, errors.New("exhausted pool unexpectedly returned a session handle")
	}
	if !errors.Is(err, sdk.ErrTimeout) {
		return result, fmt.Errorf("expected a bounded exhausted-pool timeout, got %v", err)
	}
	p.result.Warm.ExhaustionError = sessionprobe.ShortError(err)
	p.warmCheck("exhaustion_is_bounded_without_creation_replay")
	p.emit("warm-exhausted")
	if err := waitMarker(ctx, "warm-release"); err != nil {
		return result, err
	}
	for i, s := range handles {
		if err := p.stable(ctx, s, p.result.Sessions[i]); err != nil {
			return result, err
		}
		if err := s.Close(ctx); err != nil {
			return result, err
		}
		if err := p.gone(ctx, p.result.Sessions[i]); err != nil {
			return result, err
		}
		if _, err := s.Run(ctx, "echo must-not-run"); !errors.Is(err, sdk.ErrNotReady) {
			return result, errors.New("closed warm session accepted command")
		}
	}
	p.warmCheck("used_sessions_deleted_and_closed_handles_rejected")
	recovered, err := p.supply(ctx, 2)
	if err != nil {
		return result, err
	}
	if err := freshSupply(recovered, p.result.Sessions); err != nil {
		return result, err
	}
	fourth, err := client.CreateSandbox(ctx, p.pool, p.namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, fourth)
	fourthID, err := p.identify(ctx, fourth)
	if err != nil {
		return result, err
	}
	if err := allocatedFrom([]identity{fourthID}, recovered); err != nil {
		return result, err
	}
	for _, name := range []string{"private-0", "private-1", "private-2"} {
		exists, e := fourth.Exists(ctx, name)
		if e != nil || exists {
			return result, errors.New("recovered session inherited used workspace")
		}
	}
	execution, err := fourth.Run(ctx, "python3 -I -B -c 'print(42)'", sdk.WithTimeout(15*time.Second))
	if err != nil {
		return result, err
	}
	if execution == nil || execution.ExitCode != 0 || execution.Stdout != "42\n" {
		return result, errors.New("recovered session execution failed")
	}
	p.result.Sessions = append(p.result.Sessions, fourthID)
	p.emit("warm-reused")
	if err := waitMarker(ctx, "warm-return"); err != nil {
		return result, err
	}
	if err := fourth.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, fourthID); err != nil {
		return result, err
	}
	p.warmCheck("capacity_recovers_with_clean_new_environment")
	final, err := p.supply(ctx, 2)
	if err != nil {
		return result, err
	}
	if err := freshSupply(final, p.result.Sessions); err != nil {
		return result, err
	}
	remaining, err := p.dynamic.Resource(claims).Namespace(p.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, err
	}
	if len(remaining.Items) != 0 {
		return result, errors.New("claim remains after warm-session closure or failed Open")
	}
	p.result.Warm.Recovered = final
	p.warmCheck("fixed_inventory_restored_without_used_members")
	p.emit("warm-recovered")
	if err := waitMarker(ctx, "warm-finish"); err != nil {
		return result, err
	}
	// This certifies claimed-session cleanup. Host must stop the pool and verify its resources separately.
	p.result.Cleanup = true
	return p.result, nil
}
