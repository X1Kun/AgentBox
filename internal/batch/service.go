package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	yaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
)

func New(client dynamic.Interface, cfg Config) (*Service, error) {
	if client == nil || !dns.MatchString(cfg.Namespace) || !dns.MatchString(cfg.Instance) || !strings.HasPrefix(cfg.WorkerImageID, "sha256:") || !shaPattern.MatchString(strings.TrimPrefix(cfg.WorkerImageID, "sha256:")) || !strings.HasPrefix(cfg.WorkerImageDigest, "sha256:") || !shaPattern.MatchString(strings.TrimPrefix(cfg.WorkerImageDigest, "sha256:")) {
		return nil, errors.New("invalid batch configuration")
	}
	if cfg.Retention == 0 {
		cfg.Retention = 24 * time.Hour
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = 128
	}
	if cfg.Retention < time.Minute || cfg.Retention > 7*24*time.Hour || cfg.MaxRecords < 1 || cfg.MaxRecords > 1024 {
		return nil, errors.New("invalid batch retention/capacity")
	}
	data, err := os.ReadFile(cfg.TemplateFile)
	if err != nil || len(data) > 128<<10 {
		return nil, errors.New("invalid batch template file")
	}
	data, err = strictYAML(data)
	if err != nil {
		return nil, err
	}
	var job batchv1.Job
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&job) != nil {
		return nil, errors.New("invalid batch Job schema")
	}
	if err = validateTemplate(&job, cfg.Namespace); err != nil {
		return nil, err
	}
	contractData, err := os.ReadFile(cfg.ContractFile)
	if err != nil || len(contractData) > 64<<10 {
		return nil, errors.New("invalid batch contract file")
	}
	contractData, err = strictYAML(contractData)
	if err != nil {
		return nil, err
	}
	var contract Contract
	d = json.NewDecoder(bytes.NewReader(contractData))
	d.DisallowUnknownFields()
	if d.Decode(&contract) != nil || contract.Version != 1 || contract.DatasetID == "" || !shaPattern.MatchString(contract.DatasetSHA256) || len(contract.Outputs) != 8 {
		return nil, errors.New("invalid image contract")
	}
	seen := map[string]bool{}
	for _, f := range contract.Outputs {
		if !validArtifact(f.Name) || seen[f.Name] || !shaPattern.MatchString(f.PixelSHA256) || f.Width < 1 || f.Height < 1 || f.Width > 256 || f.Height > 256 {
			return nil, errors.New("invalid image output contract")
		}
		seen[f.Name] = true
	}
	encoded, _ := json.Marshal(job)
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Service{client: client, cfg: cfg, template: &job, templateHash: hash(encoded), contract: contract, store: store, now: time.Now}, nil
}
func (s *Service) Close() error { return s.store.Close() }
func (s *Service) SetTelemetry(metrics *telemetry.Metrics) {
	s.metrics = metrics
	s.store.metrics = metrics
}
func validateTemplate(job *batchv1.Job, namespace string) error {
	p := job.Spec.Template.Spec
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Namespace != namespace || job.Spec.Suspend == nil || !*job.Spec.Suspend || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 1 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 30 || *job.Spec.ActiveDeadlineSeconds > 600 || job.Spec.TTLSecondsAfterFinished != nil || job.Spec.Parallelism == nil || *job.Spec.Parallelism != 1 || job.Spec.Completions == nil || *job.Spec.Completions != 1 || job.Labels["kueue.x-k8s.io/queue-name"] != "batch" {
		return errors.New("batch template must use the fixed suspended queue and bounded retry/deadline")
	}
	if p.RuntimeClassName == nil || *p.RuntimeClassName != "agentbox-gvisor" || p.HostNetwork || p.HostPID || p.HostIPC || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.SecurityContext == nil || p.SecurityContext.RunAsUser == nil || *p.SecurityContext.RunAsUser != 1000 || p.SecurityContext.RunAsNonRoot == nil || !*p.SecurityContext.RunAsNonRoot || len(p.Containers) != 1 || len(p.InitContainers) != 1 || len(p.Volumes) != 2 || p.RestartPolicy != corev1.RestartPolicyNever {
		return errors.New("invalid batch Pod isolation")
	}
	for _, c := range append(p.Containers, p.InitContainers...) {
		if c.Image != p.Containers[0].Image || !strings.HasPrefix(c.Image, "agentbox/image-worker:") || c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem || c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged || len(c.EnvFrom) != 0 || len(c.Ports) != 0 {
			return errors.New("invalid batch container template")
		}
		if c.Resources.Requests.Cpu().MilliValue() != 500 || c.Resources.Limits.Cpu().MilliValue() != 500 || c.Resources.Requests.Memory().Value() != 256<<20 || c.Resources.Limits.Memory().Value() != 256<<20 {
			return errors.New("batch requests/limits must be 500m/256Mi")
		}
		if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Add) != 0 || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
			return errors.New("batch capabilities must be dropped")
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && (e.ValueFrom.SecretKeyRef != nil || e.ValueFrom.ConfigMapKeyRef != nil) {
				return errors.New("batch credentials are forbidden")
			}
		}
	}
	if len(p.Containers[0].VolumeMounts) != 2 || len(p.InitContainers[0].VolumeMounts) != 2 {
		return errors.New("invalid batch mounts")
	}
	main, init := p.Containers[0].VolumeMounts[0], p.InitContainers[0].VolumeMounts[0]
	if main.Name != "artifacts" || main.MountPath != "/output" || main.SubPath != "" || main.SubPathExpr != "attempts/$(JOB_UID)/$(POD_UID)" || init.Name != "artifacts" || init.MountPath != "/data" || init.SubPath != "" || init.SubPathExpr != "" {
		return errors.New("batch attempts must use isolated subpaths")
	}
	for _, v := range p.Volumes {
		if v.Name == "artifacts" {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "batch-artifacts" {
				return errors.New("invalid artifacts PVC")
			}
		} else if v.Name == "scratch" {
			if v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory || v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Value() != 16<<20 {
				return errors.New("scratch must be bounded memory")
			}
		} else {
			return errors.New("unexpected batch volume")
		}
	}
	return nil
}
func (s *Service) Ready(ctx context.Context) error {
	q, err := s.client.Resource(Queues).Namespace(s.cfg.Namespace).Get(ctx, "batch", metav1.GetOptions{})
	if err != nil {
		return err
	}
	conditions, _, _ := unstructured.NestedSlice(q.Object, "status", "conditions")
	active := false
	for _, v := range conditions {
		c := v.(map[string]interface{})
		active = active || c["type"] == "Active" && c["status"] == "True"
	}
	if !active {
		return fail("queue_unavailable")
	}
	p, err := s.client.Resource(PVCs).Namespace(s.cfg.Namespace).Get(ctx, "batch-artifacts", metav1.GetOptions{})
	if err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
	if phase != "Bound" {
		return fail("storage_unavailable")
	}
	unlock, err := s.store.lock(ctx, "installation")
	if err != nil {
		return err
	}
	defer unlock()
	expected, _ := json.Marshal(map[string]string{"instance": s.cfg.Instance, "namespace": s.cfg.Namespace, "queue_uid": string(q.GetUID()), "pvc_uid": string(p.GetUID()), "template": s.templateHash, "dataset": s.contract.DatasetSHA256, "worker_image_id": s.cfg.WorkerImageID, "worker_image_digest": s.cfg.WorkerImageDigest})
	data, err := boundedFile(s.store.root, "installation.json", 4096)
	if errors.Is(err, os.ErrNotExist) {
		return atomicWrite(s.store.root, "installation.json", expected)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return fail("installation_changed")
	}
	return nil
}
func (s *Service) Submit(ctx context.Context, principal string, request Request) (View, bool, error) {
	if !requestID.MatchString(request.RequestID) || request.DatasetID != s.contract.DatasetID || request.Passes < 0 || request.Passes > 4096 {
		return View{}, false, fail("invalid_request")
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
	global, err := s.store.lock(ctx, "catalog")
	if err != nil {
		return View{}, false, err
	}
	defer global()
	if _, readErr := s.store.read(name); errors.Is(readErr, os.ErrNotExist) {
		names, countErr := s.store.names()
		if countErr != nil {
			return View{}, false, countErr
		}
		if len(names) >= s.cfg.MaxRecords {
			return View{}, false, fail("record_capacity")
		}
	} else if readErr != nil {
		return View{}, false, readErr
	}
	unlock, err := s.store.lock(ctx, name)
	if err != nil {
		return View{}, false, err
	}
	defer unlock()
	r, err := s.store.read(name)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		names, e := s.store.names()
		if e != nil {
			return View{}, false, e
		}
		if len(names) >= s.cfg.MaxRecords {
			return View{}, false, fail("record_capacity")
		}
		r = &Record{Version: 1, Name: name, Principal: principal, Request: request, Digest: requestDigest, Template: s.templateHash, State: "submitting"}
		if err = s.store.write(r); err != nil {
			return View{}, false, err
		}
		created = true
	} else if err != nil {
		return View{}, false, err
	}
	if r.Principal != principal {
		return View{}, false, fail("not_found")
	}
	if r.Digest != requestDigest || r.Template != s.templateHash {
		return View{}, false, fail("request_conflict")
	}
	if r.State == "rejected" {
		r.State = "submitting"
		r.Reason = ""
	}
	if err = s.observe(ctx, r); err != nil {
		return View{}, false, err
	}
	return s.view(ctx, r), created, nil
}
func (s *Service) lookup(ctx context.Context, principal, id string) (*Record, func(), error) {
	parts := taskID.FindStringSubmatch(id)
	if parts == nil {
		return nil, nil, fail("not_found")
	}
	// Unknown IDs must not allocate lock files. The record is read again after
	// locking so this early existence/owner check is not an authorization cache.
	preview, readErr := s.store.read(parts[1])
	if errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, fail("not_found")
	}
	if readErr != nil {
		return nil, nil, readErr
	}
	if preview.Principal != principal || preview.JobUID != parts[2] || preview.Template != s.templateHash {
		return nil, nil, fail("not_found")
	}
	unlock, err := s.store.lock(ctx, parts[1])
	if err != nil {
		return nil, nil, err
	}
	r, err := s.store.read(parts[1])
	if errors.Is(err, os.ErrNotExist) {
		err = fail("not_found")
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	if r.Principal != principal || r.JobUID != parts[2] || r.Template != s.templateHash {
		unlock()
		return nil, nil, fail("not_found")
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
	if r.State != "succeeded" && r.State != "failed" && r.State != "cancelled" {
		r.Cancel = true
		r.State = "cancelling"
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
func (s *Service) job(ctx context.Context, r *Record) (*batchv1.Job, error) {
	u, err := s.client.Resource(Jobs).Namespace(s.cfg.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var j batchv1.Job
	if runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &j) != nil {
		return nil, fail("job_identity_changed")
	}
	if j.Labels[InstanceKey] != s.cfg.Instance || j.Labels[PrincipalKey] != principalHash(r.Principal) || j.Annotations[RequestKey] != r.Digest || j.Annotations[TemplateKey] != r.Template || r.JobUID != "" && string(j.UID) != r.JobUID {
		return nil, fail("job_identity_changed")
	}
	return &j, nil
}
func (s *Service) createJob(ctx context.Context, r *Record) (*batchv1.Job, error) {
	if r.Attempted {
		return nil, fail("submission_outcome_unknown")
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
			r.State = "rejected"
			r.Reason = "submission_rejected"
			if writeErr := s.store.write(r); writeErr != nil {
				return nil, writeErr
			}
			return nil, fail("submission_rejected")
		}
		return nil, err
	}
	return s.job(ctx, r)
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
		return nil, fail("unexpected_attempts")
	}
	var values []corev1.Pod
	for _, obj := range list.Items {
		var p corev1.Pod
		if runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &p) != nil || !controlledBy(&p, r.JobUID) || p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "agentbox-gvisor" || p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken || len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != s.template.Spec.Template.Spec.Containers[0].Image {
			return nil, fail("pod_identity_changed")
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
	list, err := s.client.Resource(Workloads).Namespace(s.cfg.Namespace).List(ctx, metav1.ListOptions{Limit: 100})
	if err != nil || list.GetContinue() != "" {
		return false
	}
	for _, o := range list.Items {
		if controlledBy(&o, r.JobUID) {
			return false
		}
	}
	return true
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
			r.State = "cancelled"
		}
		if r.State != "succeeded" && r.State != "failed" && r.State != "cancelled" {
			r.State = "unknown"
			r.Reason = "job_missing"
		}
		return s.store.write(r)
	}
	if err != nil {
		return err
	}
	if r.JobUID == "" {
		if !uid.MatchString(string(j.UID)) {
			return fail("job_identity_changed")
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
			r.State = "cancelling"
		}
		return s.store.write(r)
	}
	if r.State == "succeeded" || r.State == "failed" {
		return s.setTTL(ctx, j)
	}
	if jobCondition(j, batchv1.JobComplete) {
		pods, e := s.pods(ctx, r)
		if e != nil {
			return e
		}
		var success *corev1.Pod
		for i := range pods {
			p := &pods[i]
			if p.Status.Phase == corev1.PodSucceeded {
				if success != nil {
					return fail("unexpected_attempts")
				}
				success = p
			}
		}
		if success == nil || len(success.Status.ContainerStatuses) != 1 || success.Status.ContainerStatuses[0].State.Terminated == nil || success.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
			return fail("result_unconfirmed")
		}
		status := success.Status.ContainerStatuses[0]
		actualImageID := strings.TrimPrefix(status.ImageID, "docker-pullable://")
		if at := strings.LastIndex(actualImageID, "@"); at >= 0 {
			actualImageID = actualImageID[at+1:]
		}
		if actualImageID != s.cfg.WorkerImageID && actualImageID != s.cfg.WorkerImageDigest {
			return fail("image_identity_changed")
		}
		if err = s.publish(r, success); err != nil {
			if Code(err) != "artifact_invalid" {
				return err
			}
			r.State = "failed"
			r.Reason = "artifact_invalid"
			if e = s.store.write(r); e != nil {
				return e
			}
			return s.setTTL(ctx, j)
		}
		r.State = "succeeded"
		r.ExpiresAt = s.now().Add(s.cfg.Retention).UTC().Format(time.RFC3339)
		if err = s.store.write(r); err != nil {
			return err
		}
		return s.setTTL(ctx, j)
	}
	if jobCondition(j, batchv1.JobFailed) {
		r.State = "failed"
		r.Reason = "execution_failed"
		if err = s.store.write(r); err != nil {
			return err
		}
		return s.setTTL(ctx, j)
	}
	r.State = "running"
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		r.State = "queued"
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
		if e == nil && r.Template == s.templateHash && r.State != "rejected" {
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
func decodeOne(data []byte, value interface{}) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return fail("invalid_request")
	}
	return nil
}

func strictYAML(data []byte) ([]byte, error) {
	var value map[string]interface{}
	if err := yaml.UnmarshalStrict(data, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
