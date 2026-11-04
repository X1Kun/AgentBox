package batch

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
)

func TestPinnedWorkerTemplateRequiresConfiguredDigest(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/agentbox/batch-images/image-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = strictYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	var original batchv1.Job
	if err = json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	for _, tc := range []struct {
		image    string
		accepted bool
	}{
		{"agentbox/image-worker:local", true},
		{"docker.io/agentbox/image-worker@" + digest, true},
		{"registry.example:5000/trusted/worker@" + digest, true},
		{"registry.example/worker:latest", false},
		{"docker.io/agentbox/image-worker@sha256:" + strings.Repeat("2", 64), false},
		{"agentbox/image-worker:local@sha256:" + strings.Repeat("2", 64), false},
		{"invalid//worker@" + digest, false},
	} {
		t.Run(tc.image, func(t *testing.T) {
			job := original.DeepCopy()
			job.Spec.Template.Spec.Containers[0].Image = tc.image
			job.Spec.Template.Spec.InitContainers[0].Image = tc.image
			if err := validateTemplate(job, job.Namespace, digest); (err == nil) != tc.accepted {
				t.Fatalf("reference validation: %v", err)
			}
		})
	}
}
