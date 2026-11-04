package session

import (
	"context"
	"encoding/json"
	"github.com/X1Kun/AgentBox/internal/requestid"
	sessionprotocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"log/slog"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"strconv"
	"time"
)

func (s *Service) request(principal string, r CreateRequest) (string, int, string, error) {
	if !ValidPrincipal(principal) || !requestPattern.MatchString(r.RequestID) {
		return "", 0, "", fail(sessionprotocol.CodeInvalidRequest)
	}
	ttl := r.TTLSeconds
	if ttl == 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl < 60 || ttl > s.cfg.MaxTTL {
		return "", 0, "", fail(sessionprotocol.CodeInvalidRequest)
	}
	name := "ses-" + hash(s.cfg.Instance + "\x00" + principal + "\x00" + r.RequestID)[:40]
	encoded, _ := json.Marshal(struct {
		Pool string
		TTL  int
	}{s.cfg.Pool, ttl})
	return name, ttl, hash(string(encoded)), nil
}

// Create invokes one named create action. A lost reply is resolved by the same stable name.
func (s *Service) Create(ctx context.Context, principal string, r CreateRequest) (View, bool, error) {
	name, ttl, digest, err := s.request(principal, r)
	if err != nil {
		return View{}, false, err
	}
	c, err := s.getResource(ctx, Claims, name)
	replay := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return View{}, false, err
	}
	if apierrors.IsNotFound(err) {
		pool, e := s.getResource(ctx, Pools, s.cfg.Pool)
		if e != nil {
			return View{}, false, e
		}
		if pool.GetUID() == "" {
			return View{}, false, fail(sessionprotocol.CodeIntegrityError)
		}
		deadline := metav1.NewTime(s.now().UTC().Add(time.Duration(ttl) * time.Second).Truncate(time.Second))
		typed := &ext.SandboxClaim{TypeMeta: metav1.TypeMeta{APIVersion: "extensions.agents.x-k8s.io/v1beta1", Kind: "SandboxClaim"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.cfg.Namespace, Labels: map[string]string{ownerKey: hash(principal)[:32], instanceKey: s.cfg.Instance, managedKey: "agentbox-api"},
				Annotations: map[string]string{digestKey: digest, ttlKey: strconv.Itoa(ttl), poolKey: string(pool.GetUID())}},
			Spec: ext.SandboxClaimSpec{WarmPoolRef: ext.SandboxWarmPoolRef{Name: s.cfg.Pool}, Lifecycle: &ext.Lifecycle{ShutdownTime: &deadline, ShutdownPolicy: ext.ShutdownPolicyRetain}}}
		object, e := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
		if e != nil {
			return View{}, false, e
		}
		done := s.beginBackend(ctx, "claim_create", name)
		c, err = s.resource(Claims).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
		done(err)
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsTooManyRequests(err) {
				return View{}, false, fail(sessionprotocol.CodeAdmissionRejected)
			}
			// No second application-level create, even on an ambiguous transport failure.
			c, e = s.getResource(ctx, Claims, name)
			if e != nil {
				return View{}, false, &Error{Code: "create_outcome_unknown"}
			}
			replay = true
		}
	}
	if err := s.owned(c, principal); err != nil {
		return View{}, false, err
	}
	if c.GetAnnotations()[digestKey] != digest {
		return View{}, false, fail(sessionprotocol.CodeRequestConflict)
	}
	// Effective TTL/expiry is preserved on replay, including closed records.
	v, e := s.Get(ctx, principal, sessionID(c))
	return v, replay, e
}

// observation is local to one fresh API observation; it is never cached across
// connection checks or requests. The Pod has passed the full backing checks.
type observation struct {
	View
	pod *unstructured.Unstructured
}

func (s *Service) Get(ctx context.Context, principal, id string) (View, error) {
	started := time.Now()
	snapshot, err := s.snapshot(ctx, principal, id)
	reason := snapshot.Reason
	if reason == "" {
		reason = string(snapshot.State)
	}
	if err != nil {
		reason = Code(err)
	}
	s.metrics.Diagnostic("state_observe", reason, time.Since(started))
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "session observed", "request_id", requestid.From(ctx), "session_id", id, "state", snapshot.State, "reason", reason, "duration_ms", time.Since(started).Milliseconds())
	return snapshot.View, err
}

func (s *Service) snapshot(ctx context.Context, principal, id string) (observation, error) {
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return observation{}, err
		}
		v, change, err := s.observe(ctx, c)
		if err != nil {
			return observation{}, err
		}
		if len(change) == 0 {
			return v, nil
		}
		if err = s.patch(ctx, c, change, ""); err != nil {
			if retryPatch(err) {
				continue
			}
			return observation{}, err
		}
		// The observation becomes publishable only after its UID/version CAS
		// succeeds. Re-listing cannot make that earlier snapshot atomic.
		return v, nil
	}
	return observation{}, fail(sessionprotocol.CodeStateConflict)
}

func (s *Service) observe(ctx context.Context, c *unstructured.Unstructured) (observation, map[string]string, error) {
	expires, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownTime")
	deadline, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return observation{}, nil, fail(sessionprotocol.CodeIntegrityError)
	}
	b, err := readBinding(c)
	if err != nil {
		return observation{}, nil, err
	}
	v := observation{View: View{ID: sessionID(c), State: sessionprotocol.StateCreating, ExpiresAt: expires, IdentityRevision: revision(b), OperationActive: c.GetAnnotations()[operationKey] != ""}}
	if c.GetDeletionTimestamp() != nil {
		return observation{}, nil, fail(sessionprotocol.CodeIdentityChanged)
	}
	closing := c.GetAnnotations()[closeKey] == "true" || !s.now().Before(deadline)
	closedAt := c.GetAnnotations()[closedAtKey]
	if closedAt != "" {
		at, e := time.Parse(time.RFC3339Nano, closedAt)
		created := c.GetCreationTimestamp()
		if e != nil || at.After(s.now()) || (!created.IsZero() && at.Before(created.Time)) || !closing {
			return observation{}, nil, fail(sessionprotocol.CodeIntegrityError)
		}
		v.ClosedAt = closedAt
	}
	if closing {
		v.State = sessionprotocol.StateClosing
		v.Reason = reasonAwaitingUpstreamCleanup
		if !condition(c, "Ready", "False", ext.ClaimExpiredReason) {
			return v, nil, nil
		}
		remaining, e := s.remaining(ctx, c, b)
		if e != nil {
			return observation{}, nil, e
		}
		if !remaining {
			v.State = sessionprotocol.StateClosed
			v.Reason = ""
			v.CleanupComplete = true
			change := map[string]string{}
			if closedAt == "" {
				v.ClosedAt = s.now().UTC().Format(time.RFC3339Nano)
				change[closedAtKey] = v.ClosedAt
			}
			if v.OperationActive {
				v.OperationActive = false
				change[operationKey] = ""
			}
			return v, change, nil
		}
		return v, nil, nil
	}
	if reason := c.GetAnnotations()[invalidKey]; reason != "" {
		v.State = sessionprotocol.StateInvalidated
		v.Reason = reason
		return v, nil, nil
	}
	current, ready, reason, e := s.backing(ctx, c)
	if e != nil {
		return observation{}, nil, e
	}
	if current != nil {
		v.pod = current.pod
	}
	if b != nil {
		if current == nil || *current.binding != *b {
			v.State = sessionprotocol.StateInvalidated
			v.Reason = reasonBackingIdentityChanged
			return v, map[string]string{invalidKey: v.Reason}, nil
		}
		if reason == "backing_terminating" || reason == "backing_failed" {
			v.State = sessionprotocol.StateInvalidated
			v.Reason = reasonBackingEnvironmentTerminated
			return v, map[string]string{invalidKey: v.Reason}, nil
		}
		if !ready {
			v.State = sessionprotocol.StateUnavailable
			if reason == "backing_failed" {
				v.State = "failed"
			}
			v.Reason = reason
			return v, nil, nil
		}
		v.State = sessionprotocol.StateReady
		return v, nil, nil
	}
	if !ready {
		if reason == "backing_failed" {
			v.State = "failed"
		}
		v.Reason = reason
		return v, nil, nil
	}
	v.State = sessionprotocol.StateReady
	v.IdentityRevision = revision(current.binding)
	raw, _ := json.Marshal(current.binding)
	return v, map[string]string{bindingKey: string(raw)}, nil
}

func (s *Service) Close(ctx context.Context, principal, id string) (View, error) {
	patched := false
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return View{}, err
		}
		if c.GetAnnotations()[closeKey] == "true" {
			patched = true
			break
		}
		expires, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownTime")
		old, e := time.Parse(time.RFC3339, expires)
		if e != nil {
			return View{}, fail(sessionprotocol.CodeIntegrityError)
		}
		deadline := s.now().UTC().Truncate(time.Second)
		if old.Before(deadline) {
			deadline = old
		}
		err = s.patch(ctx, c, map[string]string{closeKey: "true"}, deadline.Format(time.RFC3339))
		if err == nil {
			patched = true
			break
		}
		if !retryPatch(err) {
			return View{}, err
		}
	}
	if !patched {
		return View{}, fail(sessionprotocol.CodeStateConflict)
	}
	end := s.now().Add(s.cfg.CloseWait)
	for {
		v, err := s.Get(ctx, principal, id)
		if err != nil {
			return View{}, err
		}
		if v.State == sessionprotocol.StateClosed || s.cfg.CloseWait == 0 || !s.now().Before(end) {
			return v, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return View{}, ctx.Err()
		case <-timer.C:
		}
	}
}
