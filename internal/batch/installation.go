package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"os"
)

func (s *Service) Ready(ctx context.Context) error {
	q, err := s.validateInstallation(ctx, true)
	if err != nil {
		return err
	}
	conditions, _, _ := unstructured.NestedSlice(q.Object, "status", "conditions")
	for _, v := range conditions {
		c, ok := v.(map[string]interface{})
		if ok && c["type"] == "Active" && c["status"] == "True" {
			return nil
		}
	}
	return fail(batchprotocol.CodeQueueUnavailable)
}

// Inactivity prevents new admission, but never changes the installation identity.
// Maintenance may verify an existing binding; it may not silently adopt storage.

func (s *Service) validateInstallation(ctx context.Context, initialize bool) (*unstructured.Unstructured, error) {
	q, err := s.client.Resource(Queues).Namespace(s.cfg.Namespace).Get(ctx, "batch", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	p, err := s.client.Resource(PVCs).Namespace(s.cfg.Namespace).Get(ctx, "batch-artifacts", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
	if phase != "Bound" {
		return nil, fail(batchprotocol.CodeStorageUnavailable)
	}
	unlock, err := s.store.lock(ctx, "installation")
	if err != nil {
		return nil, err
	}
	defer unlock()
	expected, _ := json.Marshal(map[string]string{"instance": s.cfg.Instance, "namespace": s.cfg.Namespace, "queue_uid": string(q.GetUID()), "pvc_uid": string(p.GetUID()), "template": s.templateHash, "dataset": s.contract.DatasetSHA256, "worker_image_id": s.cfg.WorkerImageID, "worker_image_digest": s.cfg.WorkerImageDigest})
	data, err := boundedFile(s.store.root, "installation.json", 4096)
	if errors.Is(err, os.ErrNotExist) {
		if !initialize {
			return nil, fail(batchprotocol.CodeInstallationUninitialized)
		}
		names, listErr := s.store.names()
		if listErr != nil {
			return nil, listErr
		}
		if len(names) != 0 {
			return nil, fail(batchprotocol.CodeInstallationUninitialized)
		}
		return q, atomicWrite(s.store.root, "installation.json", expected)
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, expected) {
		return nil, fail(batchprotocol.CodeInstallationChanged)
	}
	return q, nil
}
