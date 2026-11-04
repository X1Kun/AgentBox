package batch

import (
	"context"
	"encoding/json"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"strings"
	"time"
)

func (s *Service) job(ctx context.Context, r *Record) (*batchv1.Job, error) {
	u, err := s.client.Resource(Jobs).Namespace(s.cfg.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var j batchv1.Job
	if runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &j) != nil {
		return nil, fail(batchprotocol.CodeJobIdentityChanged)
	}
	if j.Labels[InstanceKey] != s.cfg.Instance || j.Labels[PrincipalKey] != principalHash(r.Principal) || j.Annotations[RequestKey] != r.Digest || j.Annotations[TemplateKey] != r.Template || r.JobUID != "" && string(j.UID) != r.JobUID {
		return nil, fail(batchprotocol.CodeJobIdentityChanged)
	}
	return &j, nil
}

func controlledBy(meta metav1.Object, jobUID string) bool {
	for _, ref := range meta.GetOwnerReferences() {
		if ref.UID == types.UID(jobUID) && ref.Kind == "Job" && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func (s *Service) pods(ctx context.Context, r *Record) ([]corev1.Pod, error) {
	list, err := s.client.Resource(Pods).Namespace(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + r.JobUID, Limit: 10})
	if err != nil {
		return nil, err
	}
	if list.GetContinue() != "" || len(list.Items) > 2 {
		return nil, fail(batchprotocol.CodeUnexpectedAttempts)
	}
	var values []corev1.Pod
	for _, obj := range list.Items {
		var p corev1.Pod
		if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &p) != nil || !controlledBy(&p, r.JobUID) || p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "agentbox-gvisor" || p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken || len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != s.template.Spec.Template.Spec.Containers[0].Image {
			return nil, fail(batchprotocol.CodePodIdentityChanged)
		}
		values = append(values, p)
	}
	return values, nil
}

func (s *Service) gone(ctx context.Context, r *Record) bool {
	if r.JobUID == "" {
		return false
	}
	_, err := s.job(ctx, r)
	if !apierrors.IsNotFound(err) {
		return false
	}
	pods, err := s.pods(ctx, r)
	if err != nil || len(pods) > 0 {
		return false
	}
	continuation := ""
	seen := map[string]bool{}
	for ctx.Err() == nil {
		list, err := s.client.Resource(Workloads).Namespace(s.cfg.Namespace).List(ctx, metav1.ListOptions{Limit: 100, Continue: continuation})
		if err != nil {
			return false
		}
		for _, o := range list.Items {
			if controlledBy(&o, r.JobUID) {
				return false
			}
		}
		continuation = list.GetContinue()
		if continuation == "" {
			return ctx.Err() == nil
		}
		if seen[continuation] {
			return false
		}
		seen[continuation] = true
	}
	return false
}

func (s *Service) view(ctx context.Context, r *Record) View {
	return View{ID: r.Name + "." + r.JobUID, State: r.State, Reason: r.Reason, CleanupComplete: s.gone(ctx, r), ArtifactExpiresAt: r.ExpiresAt, Artifacts: r.Artifacts}
}

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (s *Service) observe(ctx context.Context, r *Record) error {
	j, err := s.job(ctx, r)
	if apierrors.IsNotFound(err) && r.JobUID == "" && !r.Cancel {
		j, err = s.createJob(ctx, r)
	}
	if apierrors.IsNotFound(err) {
		if r.Cancel && s.gone(ctx, r) {
			r.State = batchprotocol.StateCancelled
		}
		if r.State != batchprotocol.StateSucceeded && r.State != batchprotocol.StateFailed && r.State != batchprotocol.StateCancelled {
			r.State = batchprotocol.StateUnknown
			r.Reason = batchprotocol.ReasonJobMissing
		}
		return s.store.write(r)
	}
	if err != nil {
		return err
	}
	if r.JobUID == "" {
		if !uid.MatchString(string(j.UID)) {
			return fail(batchprotocol.CodeJobIdentityChanged)
		}
		r.JobUID = string(j.UID)
		if err = s.store.write(r); err != nil {
			return err
		}
	}
	if r.Release {
		policy := metav1.DeletePropagationForeground
		precondition := types.UID(r.JobUID)
		err = s.client.Resource(Jobs).Namespace(s.cfg.Namespace).Delete(ctx, r.Name, metav1.DeleteOptions{PropagationPolicy: &policy, Preconditions: &metav1.Preconditions{UID: &precondition}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if r.Cancel {
			r.State = batchprotocol.StateCancelling
		}
		return s.store.write(r)
	}
	if r.State == batchprotocol.StateSucceeded || r.State == batchprotocol.StateFailed {
		return s.setTTL(ctx, j)
	}
	if jobCondition(j, batchv1.JobComplete) {
		return s.observeCompleted(ctx, r, j)
	}

	if jobCondition(j, batchv1.JobFailed) {
		r.State = batchprotocol.StateFailed
		r.Reason = batchprotocol.ReasonExecutionFailed
		if err = s.store.write(r); err != nil {
			return err
		}
		return s.setTTL(ctx, j)
	}
	r.State = batchprotocol.StateRunning
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		r.State = batchprotocol.StateQueued
	}
	return s.store.write(r)
}

func (s *Service) setTTL(ctx context.Context, j *batchv1.Job) error {
	if j.Spec.TTLSecondsAfterFinished != nil {
		return nil
	}
	body, _ := json.Marshal([]map[string]interface{}{{"op": "test", "path": "/metadata/uid", "value": string(j.UID)}, {"op": "add", "path": "/spec/ttlSecondsAfterFinished", "value": 600}})
	_, err := s.client.Resource(Jobs).Namespace(s.cfg.Namespace).Patch(ctx, j.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	return err
}

func (s *Service) observeCompleted(ctx context.Context, r *Record, j *batchv1.Job) error {
	var err error
	pods, e := s.pods(ctx, r)
	if e != nil {
		return e
	}
	var success *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded {
			if success != nil {
				return fail(batchprotocol.CodeUnexpectedAttempts)
			}
			success = p
		}
	}
	if success == nil || len(success.Status.ContainerStatuses) != 1 || success.Status.ContainerStatuses[0].State.Terminated == nil || success.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
		return fail(batchprotocol.CodeResultUnconfirmed)
	}
	status := success.Status.ContainerStatuses[0]
	actualImageID := strings.TrimPrefix(status.ImageID, "docker-pullable://")
	if at := strings.LastIndex(actualImageID, "@"); at >= 0 {
		actualImageID = actualImageID[at+1:]
	}
	if actualImageID != s.cfg.WorkerImageID && actualImageID != s.cfg.WorkerImageDigest {
		return fail(batchprotocol.CodeImageIdentityChanged)
	}
	if err = s.publish(r, success); err != nil {
		if Code(err) != batchprotocol.CodeArtifactInvalid {
			return err
		}
		r.State = batchprotocol.StateFailed
		r.Reason = batchprotocol.CodeArtifactInvalid
		if e = s.store.write(r); e != nil {
			return e
		}
		return s.setTTL(ctx, j)
	}
	r.State = batchprotocol.StateSucceeded
	r.ExpiresAt = s.now().Add(s.cfg.Retention).UTC().Format(time.RFC3339)
	if err = s.store.write(r); err != nil {
		return err
	}
	return s.setTTL(ctx, j)
}
