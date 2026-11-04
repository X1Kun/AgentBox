package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	"io"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	yaml "k8s.io/apimachinery/pkg/util/yaml"
	"strings"
)

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

func decodeOne(data []byte, value interface{}) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return fail(batchprotocol.CodeInvalidRequest)
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
