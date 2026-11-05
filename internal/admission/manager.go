package admission

import (
	"context"
	"errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sync/atomic"
	"time"
)

func WorkloadObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: Workloads.Group, Version: Workloads.Version, Kind: "Workload"})
	return o
}
func CheckObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: Checks.Group, Version: Checks.Version, Kind: "AdmissionCheck"})
	return o
}

type pass struct {
	at    time.Time
	valid bool
}
type managed struct {
	backend  *Controller
	latest   atomic.Pointer[pass]
	interval time.Duration
	budget   time.Duration
}

func (m *managed) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	started := time.Now()
	call, cancel := context.WithTimeout(ctx, m.budget)
	defer cancel()
	err := m.backend.Step(call)
	m.latest.Store(&pass{started, err == nil})
	if err != nil && ctx.Err() == nil {
		reason := "backend_unavailable"
		switch {
		case apierrors.IsConflict(err) || apierrors.IsInvalid(err):
			reason = "state_conflict"
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			reason = "backend_forbidden"
		case errors.Is(err, context.DeadlineExceeded):
			reason = "backend_timeout"
		}
		m.backend.logger.Warn("admission reconcile failed", "reason", reason)
	}
	// Errors are deliberately not handed to controller-runtime's logger: native
	// Kubernetes/transport errors can contain endpoints and response bodies.
	return ctrl.Result{RequeueAfter: m.interval}, nil
}
func (m *managed) ready(_ *http.Request) error {
	v := m.latest.Load()
	if v == nil || !v.valid || time.Since(v.at) < 0 || time.Since(v.at) > m.budget+3*m.interval {
		return errors.New("admission observation unavailable")
	}
	return nil
}

type tickSource struct {
	events   chan event.GenericEvent
	interval time.Duration
	object   client.Object
	managed  *managed
}

func (*tickSource) NeedLeaderElection() bool { return true }
func (s *tickSource) Start(ctx context.Context) error {
	defer s.managed.latest.Store(nil)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case s.events <- event.GenericEvent{Object: s.object}:
		case <-ctx.Done():
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Controller) Setup(manager ctrl.Manager) error {
	m := &managed{backend: c, interval: seconds(c.cfg.PollSeconds), budget: seconds(c.cfg.RequestTimeoutSeconds * 4)}
	key := types.NamespacedName{Namespace: c.cfg.Namespace, Name: "health-domain"}
	mapDomain := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: key}}
	})
	events := make(chan event.GenericEvent, 1)
	if err := ctrl.NewControllerManagedBy(manager).Named("agentbox-health-admission").
		Watches(WorkloadObject(), mapDomain).
		Watches(CheckObject(), mapDomain).
		WatchesRawSource(source.Channel(events, &handler.EnqueueRequestForObject{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(m); err != nil {
		return err
	}
	if err := manager.AddReadyzCheck("admission", m.ready); err != nil {
		return err
	}
	return manager.Add(&tickSource{events: events, interval: m.interval, object: &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": key.Name, "namespace": key.Namespace, "creationTimestamp": metav1.Now().Format(time.RFC3339)}}}, managed: m})
}
