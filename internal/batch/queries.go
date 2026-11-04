package batch

import (
	"context"
	"errors"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	"os"
)

func (s *Service) lookup(ctx context.Context, principal, id string) (*Record, func(), error) {
	parts := taskID.FindStringSubmatch(id)
	if parts == nil {
		return nil, nil, fail(batchprotocol.CodeNotFound)
	}
	// Unknown IDs must not allocate lock files. The record is read again after
	// locking so this early existence/owner check is not an authorization cache.
	preview, readErr := s.store.read(parts[1])
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, fail(batchprotocol.CodeNotFound)
	}
	if readErr != nil {
		return nil, nil, readErr
	}
	if preview.Principal != principal || preview.JobUID != parts[2] || preview.Template != s.templateHash {
		return nil, nil, fail(batchprotocol.CodeNotFound)
	}
	unlock, err := s.store.lock(ctx, parts[1])
	if err != nil {
		return nil, nil, err
	}
	r, err := s.store.read(parts[1])
	if errors.Is(err, os.ErrNotExist) {
		err = fail(batchprotocol.CodeNotFound)
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	if r.Principal != principal || r.JobUID != parts[2] || r.Template != s.templateHash {
		unlock()
		return nil, nil, fail(batchprotocol.CodeNotFound)
	}
	return r, unlock, nil
}

func (s *Service) Get(ctx context.Context, principal, id string) (View, error) {
	r, unlock, err := s.lookup(ctx, principal, id)
	if err != nil {
		return View{}, err
	}
	defer unlock()
	if err = s.observe(ctx, r); err != nil {
		return View{}, err
	}
	return s.view(ctx, r), nil
}

func (s *Service) Cancel(ctx context.Context, principal, id string) (View, error) {
	r, unlock, err := s.lookup(ctx, principal, id)
	if err != nil {
		return View{}, err
	}
	defer unlock()
	if r.State != batchprotocol.StateSucceeded && r.State != batchprotocol.StateFailed && r.State != batchprotocol.StateCancelled {
		r.Cancel = true
		r.State = batchprotocol.StateCancelling
	}
	r.Release = true
	if err = s.store.write(r); err != nil {
		return View{}, err
	}
	if err = s.observe(ctx, r); err != nil {
		return View{}, err
	}
	return s.view(ctx, r), nil
}
