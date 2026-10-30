package session

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var ResourceQuotas = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}

const claimQuotaKey = "count/sandboxclaims.extensions.agents.x-k8s.io"

type CleanupOptions struct {
	Apply      bool
	Retention  time.Duration
	MaxRecords int
	QuotaName  string
}
type RecordQuota struct {
	Name     string `json:"name"`
	Known    bool   `json:"known"`
	Hard     int64  `json:"hard,omitempty"`
	Used     int64  `json:"used,omitempty"`
	Pressure string `json:"pressure"` // unknown, normal, warning, exhausted
}
type CleanupEntry struct {
	ID              string   `json:"session_id"`
	UID             string   `json:"uid"`
	ResourceVersion string   `json:"resource_version"`
	Owner           string   `json:"owner_digest"`
	RequestDigest   string   `json:"request_digest"`
	PoolUID         string   `json:"pool_uid"`
	Binding         *Binding `json:"binding,omitempty"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	ClosedAt        string   `json:"closed_at,omitempty"`
	Outcome         string   `json:"outcome"`
}
type CleanupReport struct {
	Version          int            `json:"schema_version"`
	Namespace        string         `json:"namespace"`
	Pool             string         `json:"pool"`
	Instance         string         `json:"instance"`
	Owner            string         `json:"owner_digest"`
	Apply            bool           `json:"apply"`
	RetentionSeconds int64          `json:"retention_seconds"`
	StartedAt        string         `json:"started_at"`
	FinishedAt       string         `json:"finished_at,omitempty"`
	Truncated        bool           `json:"truncated"`
	Quota            *RecordQuota   `json:"quota_before,omitempty"`
	Entries          []CleanupEntry `json:"entries"`
	Error            string         `json:"error,omitempty"`
}

// Audit must durably save the complete report before returning nil. Apply
// refuses to mutate without it, and stops on any audit or uncertain API error.
// No permanent controller or new lifecycle store is involved.
func (s *Service) Sweep(ctx context.Context, principal string, opts CleanupOptions, audit func(CleanupReport) error) (report CleanupReport, err error) {
	if opts.Retention == 0 {
		opts.Retention = 24 * time.Hour
	}
	if opts.MaxRecords == 0 {
		opts.MaxRecords = 100
	}
	if !ValidPrincipal(principal) || opts.Retention < time.Second || opts.Retention > 365*24*time.Hour || opts.Retention%time.Second != 0 || opts.MaxRecords < 1 || opts.MaxRecords > 1000 || (opts.QuotaName != "" && !dns.MatchString(opts.QuotaName)) || (opts.Apply && audit == nil) {
		return report, fail("invalid_cleanup_configuration")
	}
	report = CleanupReport{Version: 1, Namespace: s.cfg.Namespace, Pool: s.cfg.Pool, Instance: s.cfg.Instance, Owner: hash(principal)[:32], Apply: opts.Apply, RetentionSeconds: int64(opts.Retention / time.Second), StartedAt: s.now().UTC().Format(time.RFC3339Nano), Entries: []CleanupEntry{}}
	save := func() error {
		if audit != nil && audit(report) != nil {
			return fail("audit_failed")
		}
		return nil
	}
	defer func() {
		report.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			report.Error = Code(err)
		}
		if e := save(); e != nil {
			err = e
			report.Error = Code(e)
		}
	}()
	if opts.QuotaName != "" {
		report.Quota, err = s.recordQuota(ctx, opts.QuotaName)
		if err != nil {
			return report, fail("quota_unavailable")
		}
	}
	selector := labels.Set{ownerKey: report.Owner, instanceKey: s.cfg.Instance, managedKey: "agentbox-api"}.AsSelector().String()
	listed, e := s.resource(Claims).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: int64(opts.MaxRecords)})
	if e != nil {
		return report, fail("cleanup_list_failed")
	}
	report.Truncated = listed.GetContinue() != "" || len(listed.Items) > opts.MaxRecords
	for i := 0; i < len(listed.Items) && i < opts.MaxRecords; i++ {
		if ctx.Err() != nil {
			return report, fail("cleanup_timeout")
		}
		candidate := &listed.Items[i]
		entry := CleanupEntry{ID: sessionID(candidate), UID: string(candidate.GetUID()), ResourceVersion: candidate.GetResourceVersion(), Owner: candidate.GetLabels()[ownerKey], RequestDigest: candidate.GetAnnotations()[digestKey], PoolUID: candidate.GetAnnotations()[poolKey]}
		report.Entries = append(report.Entries, entry)
		index := len(report.Entries) - 1
		current, loadErr := s.load(ctx, principal, entry.ID)
		if loadErr != nil && Code(loadErr) == "backend_unavailable" {
			report.Entries[index].Outcome = "read_failed"
			return report, fail("cleanup_read_failed")
		}
		if loadErr != nil || current.GetResourceVersion() != entry.ResourceVersion {
			report.Entries[index].Outcome = "skip_changed_or_unowned"
			continue
		}
		v, change, observeErr := s.observe(ctx, current)
		if observeErr != nil {
			report.Entries[index].Outcome = "skip_unverifiable"
			if Code(observeErr) == "backend_unavailable" {
				return report, fail("cleanup_read_failed")
			}
			continue
		}
		report.Entries[index].ExpiresAt = v.ExpiresAt
		report.Entries[index].ClosedAt = v.ClosedAt
		report.Entries[index].Binding, _ = readBinding(current)
		if v.State != "closed" || !v.CleanupComplete {
			report.Entries[index].Outcome = "skip_not_closed"
			continue
		}
		// Legacy records start a full retention window when first observed
		// closed, rather than backdating from creation or the shutdown time.
		if len(change) > 0 {
			report.Entries[index].Outcome = "would_finalize"
		} else {
			at, parseErr := time.Parse(time.RFC3339Nano, v.ClosedAt)
			if parseErr != nil || s.now().Before(at.Add(opts.Retention)) {
				report.Entries[index].Outcome = "retain"
				continue
			}
			report.Entries[index].Outcome = "would_delete"
		}
		if !opts.Apply {
			continue
		}
		if len(change) > 0 {
			report.Entries[index].Outcome = "finalize_planned"
		} else {
			report.Entries[index].Outcome = "delete_planned"
		}
		// The binding/identity and intended action are on disk before mutation.
		if e := save(); e != nil {
			return report, e
		}
		if ctx.Err() != nil {
			report.Entries[index].Outcome = "cancelled"
			return report, fail("cleanup_timeout")
		}
		latest, loadErr := s.load(ctx, principal, entry.ID)
		if loadErr != nil && Code(loadErr) == "backend_unavailable" {
			report.Entries[index].Outcome = "read_failed"
			return report, fail("cleanup_read_failed")
		}
		if loadErr != nil || latest.GetResourceVersion() != entry.ResourceVersion {
			report.Entries[index].Outcome = "skip_changed_or_unowned"
			continue
		}
		check, latestChange, checkErr := s.observe(ctx, latest)
		if checkErr != nil && Code(checkErr) == "backend_unavailable" {
			report.Entries[index].Outcome = "read_failed"
			return report, fail("cleanup_read_failed")
		}
		if checkErr != nil || check.State != "closed" || !check.CleanupComplete {
			report.Entries[index].Outcome = "skip_unverifiable"
			continue
		}
		if len(change) > 0 {
			// Keep the timestamp from the first confirmed observation, which
			// has already been journaled. The CAS refuses intervening writers.
			if len(latestChange) == 0 {
				report.Entries[index].Outcome = "skip_changed_or_unowned"
				continue
			}
			if ctx.Err() != nil {
				report.Entries[index].Outcome = "cancelled"
				return report, fail("cleanup_timeout")
			}
			if e := s.patch(ctx, latest, change, ""); e != nil {
				if retryPatch(e) {
					report.Entries[index].Outcome = "skip_changed_or_unowned"
				} else {
					report.Entries[index].Outcome = "finalize_outcome_unknown"
					return report, fail("cleanup_outcome_unknown")
				}
			} else {
				report.Entries[index].Outcome = "finalized"
			}
		} else {
			at, parseErr := time.Parse(time.RFC3339Nano, check.ClosedAt)
			if len(latestChange) > 0 || check.ClosedAt != v.ClosedAt || parseErr != nil || s.now().Before(at.Add(opts.Retention)) {
				report.Entries[index].Outcome = "retain"
				continue
			}
			uid, rv := latest.GetUID(), latest.GetResourceVersion()
			if ctx.Err() != nil {
				report.Entries[index].Outcome = "cancelled"
				return report, fail("cleanup_timeout")
			}
			e := s.resource(Claims).Delete(ctx, latest.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
			switch {
			case apierrors.IsNotFound(e):
				report.Entries[index].Outcome = "already_absent"
			case apierrors.IsConflict(e):
				report.Entries[index].Outcome = "skip_changed_or_unowned"
			case apierrors.IsForbidden(e) || apierrors.IsUnauthorized(e) || apierrors.IsInvalid(e) || apierrors.IsBadRequest(e):
				report.Entries[index].Outcome = "delete_rejected"
				return report, fail("cleanup_rejected")
			case e != nil:
				report.Entries[index].Outcome = "delete_outcome_unknown"
				return report, fail("cleanup_outcome_unknown")
			default:
				after, getErr := s.resource(Claims).Get(ctx, latest.GetName(), metav1.GetOptions{})
				if apierrors.IsNotFound(getErr) {
					report.Entries[index].Outcome = "deleted"
				} else if getErr != nil {
					report.Entries[index].Outcome = "delete_outcome_unknown"
					return report, fail("cleanup_outcome_unknown")
				} else if after.GetUID() != uid {
					report.Entries[index].Outcome = "original_absent"
				} else {
					report.Entries[index].Outcome = "deleting"
				}
			}
		}
		if e := save(); e != nil {
			return report, e
		}
	}
	return report, nil
}

func (s *Service) recordQuota(ctx context.Context, name string) (*RecordQuota, error) {
	obj, err := s.resource(ResourceQuotas).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	q := &RecordQuota{Name: name, Pressure: "unknown"}
	read := func(fields ...string) (int64, bool) {
		value, found, e := unstructured.NestedString(obj.Object, fields...)
		if e != nil || !found {
			return 0, false
		}
		quantity, e := resource.ParseQuantity(value)
		if e != nil {
			return 0, false
		}
		return quantity.AsInt64()
	}
	hard, h := read("spec", "hard", claimQuotaKey)
	observed, o := read("status", "hard", claimQuotaKey)
	used, u := read("status", "used", claimQuotaKey)
	if !h || !o || !u || hard <= 0 || used < 0 || observed != hard {
		return q, nil
	}
	q.Known, q.Hard, q.Used, q.Pressure = true, hard, used, "normal"
	if used >= hard {
		q.Pressure = "exhausted"
	} else if float64(used)/float64(hard) >= .8 {
		q.Pressure = "warning"
	}
	return q, nil
}
