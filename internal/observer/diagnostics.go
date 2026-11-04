package observer

import (
	"context"
	"errors"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const pollBudget = 4 * time.Second

type observationError struct {
	reason string
	status int
}

type apiStatusError struct{ code int }

func (e *apiStatusError) Error() string         { return "http_status" }
func (e *apiStatusError) Status() metav1.Status { return metav1.Status{Code: int32(e.code)} }

func (e *observationError) Error() string { return e.reason }

func readReason(err error) (string, int) {
	if err == nil {
		return "ok", 200
	}
	var classified *observationError
	if errors.As(err, &classified) {
		return classified.reason, classified.status
	}
	code := 0
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code = int(status.Status().Code)
	}
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), code == 408, code == 504:
		return "timeout", code
	case errors.Is(err, context.Canceled):
		return "canceled", code
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err), code == 401, code == 403:
		return "forbidden", code
	case apierrors.IsTooManyRequests(err), code == 429:
		return "throttled", code
	case apierrors.IsNotFound(err), code == 404:
		return "not_found", code
	case apierrors.IsConflict(err), code == 409:
		return "conflict", code
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "invalid", code
	case errors.As(err, &network) && network.Timeout():
		return "timeout", code
	default:
		return "unavailable", code
	}
}

func remainingMS(ctx context.Context) int64 {
	if deadline, ok := ctx.Deadline(); ok {
		return max(0, time.Until(deadline).Milliseconds())
	}
	return -1
}

type pollContext struct{ id, domain string }
type pollKey struct{}
