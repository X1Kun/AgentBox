package batch

import (
	"context"
	"errors"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	"sort"
	"time"
)

func (s *Service) Reconcile(ctx context.Context) (resultErr error) {
	started := time.Now()
	defer func() {
		outcome := "complete"
		if resultErr != nil {
			outcome = "error"
		}
		s.metrics.Operation("reconcile", outcome, time.Since(started))
	}()
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if _, err := s.validateInstallation(ctx, false); err != nil {
		return err
	}
	names, err := s.store.names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		s.reconcileAfter = ""
		return nil
	}
	sort.Strings(names)
	start := sort.Search(len(names), func(i int) bool { return names[i] > s.reconcileAfter })
	var failures []error
	for i := 0; i < len(names); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := names[(start+i)%len(names)]
		// Advance before taking the lock or making an API call: a slow record
		// can consume this entire pass, but cannot pin the next pass to itself.
		s.reconcileAfter = name
		unlock, e := s.store.lock(ctx, name)
		if e != nil {
			return e
		}
		r, e := s.store.read(name)
		if e == nil && r.Template == s.templateHash && r.State != batchprotocol.StateRejected {
			e = s.observe(ctx, r)
			if e == nil {
				e = s.expire(ctx, r)
			}
		}
		unlock()
		if e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
