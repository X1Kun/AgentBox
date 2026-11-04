package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"os"
)

func (s *Service) Submit(ctx context.Context, principal string, request Request) (View, bool, error) {
	if !requestID.MatchString(request.RequestID) || request.DatasetID != s.contract.DatasetID || request.Passes < 0 || request.Passes > 4096 {
		return View{}, false, fail(batchprotocol.CodeInvalidRequest)
	}
	if request.Passes == 0 {
		request.Passes = 1
	}
	if err := s.Ready(ctx); err != nil {
		return View{}, false, err
	}
	data, _ := json.Marshal(request)
	requestDigest := hash(data)
	name := "bat-" + hash([]byte(s.cfg.Instance + "\x00" + principal + "\x00" + request.RequestID))[:40]
	created, err := s.reserveRecord(ctx, name, principal, request, requestDigest)
	if err != nil {
		return View{}, false, err
	}
	unlock, err := s.store.lock(ctx, name)
	if err != nil {
		return View{}, false, err
	}
	defer unlock()
	r, err := s.store.read(name)
	if err != nil {
		return View{}, false, err
	}
	if r.Principal != principal {
		return View{}, false, fail(batchprotocol.CodeNotFound)
	}
	if r.Digest != requestDigest || r.Template != s.templateHash {
		return View{}, false, fail(batchprotocol.CodeRequestConflict)
	}
	if r.State == batchprotocol.StateRejected {
		r.State = batchprotocol.StateSubmitting
		r.Reason = ""
	}
	if err = s.observe(ctx, r); err != nil {
		return View{}, false, err
	}
	return s.view(ctx, r), created, nil
}

// reserveRecord serializes capacity reservation only. Existing records need no
// catalog lock: they cannot disappear during normal product operation. A new
// record cannot have a live record-lock holder before its durable reservation;
// acquire that lock under catalog to protect its atomic-write temporary as well.

func (s *Service) reserveRecord(ctx context.Context, name, principal string, request Request, digest string) (bool, error) {
	if _, err := s.store.read(name); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	global, err := s.store.lock(ctx, "catalog")
	if err != nil {
		return false, err
	}
	defer global()
	if _, err := s.store.read(name); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	names, err := s.store.names()
	if err != nil {
		return false, err
	}
	if len(names) >= s.cfg.MaxRecords {
		return false, fail(batchprotocol.CodeRecordCapacity)
	}
	unlock, err := s.store.lock(ctx, name)
	if err != nil {
		return false, err
	}
	defer unlock()
	r := &Record{Version: 1, Name: name, Principal: principal, Request: request, Digest: digest, Template: s.templateHash, State: batchprotocol.StateSubmitting}
	return true, s.store.write(r)
}

func (s *Service) createJob(ctx context.Context, r *Record) (*batchv1.Job, error) {
	if r.Attempted {
		return nil, fail(batchprotocol.CodeSubmissionOutcomeUnknown)
	}
	r.Attempted = true
	if err := s.store.write(r); err != nil {
		return nil, err
	}
	j := s.template.DeepCopy()
	j.Name = r.Name
	j.GenerateName = ""
	j.UID = ""
	j.ResourceVersion = ""
	if j.Labels == nil {
		j.Labels = map[string]string{}
	}
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Labels[InstanceKey] = s.cfg.Instance
	j.Labels[PrincipalKey] = principalHash(r.Principal)
	j.Annotations[RequestKey] = r.Digest
	j.Annotations[TemplateKey] = r.Template
	p := &j.Spec.Template.Spec
	p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "BATCH_REQUEST_SHA256", Value: r.Digest}, corev1.EnvVar{Name: "BATCH_PASSES", Value: fmt.Sprint(r.Request.Passes)})
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(j)
	if err != nil {
		return nil, err
	}
	_, err = s.client.Resource(Jobs).Namespace(s.cfg.Namespace).Create(ctx, &unstructured.Unstructured{Object: data}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsTooManyRequests(err) {
			r.Attempted = false
			r.State = batchprotocol.StateRejected
			r.Reason = batchprotocol.CodeSubmissionRejected
			if writeErr := s.store.write(r); writeErr != nil {
				return nil, writeErr
			}
			return nil, fail(batchprotocol.CodeSubmissionRejected)
		}
		return nil, err
	}
	return s.job(ctx, r)
}
