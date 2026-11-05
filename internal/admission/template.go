package admission

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func supportedPod(set map[string]any) bool {
	raw, found, _ := unstructured.NestedMap(set, "template", "spec")
	if !found {
		return false
	}
	var p corev1.PodSpec
	if runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &p) != nil || p.RuntimeClassName == nil || *p.RuntimeClassName != "agentbox-gvisor" || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.HostNetwork || p.HostPID || p.HostIPC || len(p.Containers) != 1 || len(p.InitContainers) > 1 || p.RestartPolicy != corev1.RestartPolicyNever {
		return false
	}
	for _, container := range append(p.Containers, p.InitContainers...) {
		if container.Resources.Requests.Cpu().MilliValue() != 500 || container.Resources.Limits.Cpu().MilliValue() != 500 || container.Resources.Requests.Memory().Value() != 256<<20 || container.Resources.Limits.Memory().Value() != 256<<20 {
			return false
		}
	}
	return true
}
