package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dfake "k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"
)

var ctx = context.Background()

type harness struct {
	t       *testing.T
	service *Service
	client  *dfake.FakeDynamicClient
	cfg     Config
	created atomic.Int32
	files   map[string][]byte
}

func setup(t *testing.T) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	initial := []runtime.Object{
		&unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": "LocalQueue", "metadata": map[string]interface{}{"name": "batch", "namespace": "agentbox-batch", "uid": "queue-1"}, "status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Active", "status": "True"}}}}},
		&unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]interface{}{"name": "batch-artifacts", "namespace": "agentbox-batch", "uid": "pvc-1"}, "status": map[string]interface{}{"phase": "Bound"}}},
	}
	client := dfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{Jobs: "JobList", Pods: "PodList", Workloads: "WorkloadList", Queues: "LocalQueueList", PVCs: "PersistentVolumeClaimList"}, initial...)
	dir := t.TempDir()
	h := &harness{t: t, client: client, files: map[string][]byte{}}
	contract := Contract{Version: 1, DatasetID: "tiny-v1", DatasetSHA256: strings.Repeat("a", 64)}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("sample-%d.png", i)
		pixels := []byte{byte(i), 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120}
		img := image.NewRGBA(image.Rect(0, 0, 2, 2))
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				n := (y*2 + x) * 3
				img.SetRGBA(x, y, color.RGBA{pixels[n], pixels[n+1], pixels[n+2], 255})
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		h.files[name] = b.Bytes()
		contract.Outputs = append(contract.Outputs, File{Name: name, PixelSHA256: hash(pixels), Width: 2, Height: 2})
	}
	data, _ := json.Marshal(contract)
	contractFile := filepath.Join(dir, "contract.json")
	if err := os.WriteFile(contractFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	h.cfg = Config{Namespace: "agentbox-batch", Instance: "test", DataDir: dir, TemplateFile: "../../deploy/agentbox/batch-images/image-job.yaml", ContractFile: contractFile, WorkerImageID: "sha256:" + strings.Repeat("1", 64), WorkerImageDigest: "sha256:" + strings.Repeat("2", 64), Retention: time.Hour, MaxRecords: 16}
	client.PrependReactor("create", "jobs", func(action kt.Action) (bool, runtime.Object, error) {
		n := h.created.Add(1)
		o := action.(kt.CreateAction).GetObject().(*unstructured.Unstructured)
		o.SetUID(types.UID(fmt.Sprintf("job-%d", n)))
		o.SetResourceVersion("1")
		return false, nil, nil
	})
	var err error
	h.service, err = New(client, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.service.Close() })
	return h
}
func (h *harness) submit(request string) View {
	h.t.Helper()
	v, _, err := h.service.Submit(ctx, "alice", Request{RequestID: request, DatasetID: "tiny-v1", Passes: 1})
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}
func (h *harness) record(id string) *Record {
	h.t.Helper()
	r, err := h.service.store.read(strings.Split(id, ".")[0])
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}
func (h *harness) job(id string) *batchv1.Job {
	h.t.Helper()
	r := h.record(id)
	j, err := h.service.job(ctx, r)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}
func (h *harness) putJob(j *batchv1.Job) {
	h.t.Helper()
	data, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(j)
	if _, err := h.client.Resource(Jobs).Namespace(h.cfg.Namespace).Update(ctx, &unstructured.Unstructured{Object: data}, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) pod(j *batchv1.Job, name string, phase corev1.PodPhase, code int32) *corev1.Pod {
	h.t.Helper()
	controller := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.Namespace, UID: types.UID(name), Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(j.UID)}, OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: j.Name, UID: j.UID, Controller: &controller}}}, Spec: *j.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "image-worker", Image: h.cfg.WorkerImageID, ImageID: h.cfg.WorkerImageID, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}}}
	u, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if _, err := h.client.Resource(Pods).Namespace(h.cfg.Namespace).Create(ctx, &unstructured.Unstructured{Object: u}, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	return p
}
func (h *harness) complete(id string, corrupt bool) {
	h.t.Helper()
	j := h.job(id)
	p := h.pod(j, "pod-success", corev1.PodSucceeded, 0)
	r := h.record(id)
	path := "attempts/" + r.JobUID + "/" + string(p.UID)
	if err := h.service.store.root.MkdirAll(path, 0770); err != nil {
		h.t.Fatal(err)
	}
	result := attemptResult{Version: 1, DatasetSHA256: h.service.contract.DatasetSHA256, RequestSHA256: r.Digest, JobUID: r.JobUID, PodUID: string(p.UID), Passes: r.Request.Passes}
	for _, f := range h.service.contract.Outputs {
		data := h.files[f.Name]
		f.Size = int64(len(data))
		f.SHA256 = hash(data)
		if err := h.service.store.root.WriteFile(path+"/"+f.Name, data, 0600); err != nil {
			h.t.Fatal(err)
		}
		result.Files = append(result.Files, f)
	}
	if corrupt {
		result.Files[0].PixelSHA256 = strings.Repeat("0", 64)
	}
	data, _ := json.Marshal(result)
	if err := h.service.store.root.WriteFile(path+"/result.json", data, 0600); err != nil {
		h.t.Fatal(err)
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	j.Status.Succeeded = 1
	h.putJob(j)
}

func TestConcurrentSubmissionIsOneJobAndConflictsAreRejected(t *testing.T) {
	h := setup(t)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "same", DatasetID: "tiny-v1"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- v.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Error("duplicate identity")
		}
	}
	if h.created.Load() != 1 {
		t.Fatal("created", h.created.Load())
	}
	if _, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "same", DatasetID: "tiny-v1", Passes: 2}); Code(err) != "request_conflict" {
		t.Fatal(err)
	}
	if _, err := h.service.Get(ctx, "bob", first); Code(err) != "not_found" {
		t.Fatal(err)
	}
}
func TestPublishedArtifactsSurviveJobDeletionAndAPIRestart(t *testing.T) {
	h := setup(t)
	v := h.submit("publish")
	h.complete(v.ID, false)
	v, err := h.service.Get(ctx, "alice", v.ID)
	if err != nil || v.State != "succeeded" || len(v.Artifacts) != 8 {
		t.Fatal(v, err)
	}
	if _, err = h.service.Download(ctx, "bob", v.ID, v.Artifacts[0].Name); Code(err) != "not_found" {
		t.Fatal(err)
	}
	if _, err = h.service.Cancel(ctx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Delete(ctx, "pod-success", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = h.service.Close()
	h.service, err = New(h.client, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.service.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := h.service.Submit(ctx, "alice", Request{RequestID: "publish", DatasetID: "tiny-v1"})
	if err != nil || created || replayed.ID != v.ID || replayed.State != "succeeded" || !replayed.CleanupComplete {
		t.Fatal(replayed, created, err)
	}
	data, err := h.service.Download(ctx, "alice", v.ID, v.Artifacts[0].Name)
	if err != nil || !bytes.Equal(data, h.files[v.Artifacts[0].Name]) {
		t.Fatal(err)
	}
	if h.created.Load() != 1 {
		t.Fatal("execution replayed")
	}
}
func TestInvalidAttemptNeverPublishesOrMasqueradesAsSuccess(t *testing.T) {
	h := setup(t)
	v := h.submit("bad")
	h.complete(v.ID, true)
	v, err := h.service.Get(ctx, "alice", v.ID)
	if err != nil || v.State != "failed" || v.Reason != "artifact_invalid" {
		t.Fatal(v, err)
	}
	if _, err = h.service.Download(ctx, "alice", v.ID, "sample-0.png"); Code(err) != "artifact_not_ready" {
		t.Fatal(err)
	}
	if _, err = h.service.store.root.Stat("published/" + strings.Split(v.ID, ".")[0]); !os.IsNotExist(err) {
		t.Fatal("invalid result published", err)
	}
}
func TestCancellationUsesUIDAndWaitsForChildren(t *testing.T) {
	h := setup(t)
	v := h.submit("cancel")
	j := h.job(v.ID)
	h.pod(j, "pod-active", corev1.PodRunning, 0)
	view, err := h.service.Cancel(ctx, "alice", v.ID)
	if err != nil || view.State != "cancelling" || view.CleanupComplete {
		t.Fatal(view, err)
	}
	var deletion kt.DeleteAction
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource() == Jobs {
			deletion = a.(kt.DeleteAction)
		}
	}
	if deletion == nil || deletion.GetDeleteOptions().Preconditions == nil || *deletion.GetDeleteOptions().Preconditions.UID != j.UID {
		t.Fatal("delete missing UID fence")
	}
	_ = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Delete(ctx, "pod-active", metav1.DeleteOptions{})
	view, err = h.service.Get(ctx, "alice", v.ID)
	if err != nil || view.State != "cancelled" || !view.CleanupComplete {
		t.Fatal(view, err)
	}
}
func TestReplacedJobCannotBeAdopted(t *testing.T) {
	h := setup(t)
	v := h.submit("replace")
	j := h.job(v.ID)
	j.UID = "replacement"
	h.putJob(j)
	if _, err := h.service.Get(ctx, "alice", v.ID); Code(err) != "job_identity_changed" {
		t.Fatal(err)
	}
}
func TestMissingBoundJobIsNeverRecreated(t *testing.T) {
	h := setup(t)
	v := h.submit("gone")
	_ = h.client.Resource(Jobs).Namespace(h.cfg.Namespace).Delete(ctx, h.record(v.ID).Name, metav1.DeleteOptions{})
	v, err := h.service.Get(ctx, "alice", v.ID)
	if err != nil || v.State != "unknown" || h.created.Load() != 1 {
		t.Fatal(v, err)
	}
}
func TestUncertainCreateResponseRecoversExistingUIDWithoutReplaying(t *testing.T) {
	h := setup(t)
	var once atomic.Bool
	h.client.PrependReactor("create", "jobs", func(a kt.Action) (bool, runtime.Object, error) {
		if once.Swap(true) {
			return false, nil, nil
		}
		o := a.(kt.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		o.SetUID("lost-job")
		if err := h.client.Tracker().Create(Jobs, o, h.cfg.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewTimeoutError("response lost", 0)
	})
	if _, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "lost", DatasetID: "tiny-v1"}); err == nil {
		t.Fatal("expected uncertain transport")
	}
	v, created, err := h.service.Submit(ctx, "alice", Request{RequestID: "lost", DatasetID: "tiny-v1"})
	if err != nil || created || !strings.HasSuffix(v.ID, ".lost-job") {
		t.Fatal(v, created, err)
	}
	count := 0
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "create" && a.GetResource() == Jobs {
			count++
		}
	}
	if count != 1 {
		t.Fatal("replayed uncertain create", count)
	}
}
func TestUnknownCreateWithoutObjectFailsClosed(t *testing.T) {
	h := setup(t)
	h.client.PrependReactor("create", "jobs", func(a kt.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTimeoutError("unknown", 0)
	})
	_, _, _ = h.service.Submit(ctx, "alice", Request{RequestID: "unknown", DatasetID: "tiny-v1"})
	if _, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "unknown", DatasetID: "tiny-v1"}); Code(err) != "submission_outcome_unknown" {
		t.Fatal(err)
	}
	count := 0
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "create" && a.GetResource() == Jobs {
			count++
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}
func TestArtifactCorruptionSymlinkAndExpirationAreRejected(t *testing.T) {
	h := setup(t)
	v := h.submit("integrity")
	h.complete(v.ID, false)
	v, err := h.service.Get(ctx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := v.Artifacts[0].Name
	path := "published/" + strings.Split(v.ID, ".")[0] + "/" + name
	if err = h.service.store.root.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = h.service.Download(ctx, "alice", v.ID, name); Code(err) != "artifact_invalid" {
		t.Fatal(err)
	}
	_ = h.service.store.root.Remove(path)
	if err = h.service.store.root.Symlink("../../installation.json", path); err != nil {
		t.Fatal(err)
	}
	if _, err = h.service.Download(ctx, "alice", v.ID, name); Code(err) != "artifact_invalid" {
		t.Fatal(err)
	}
	if _, err = h.service.Download(ctx, "alice", v.ID, "../installation.json"); Code(err) != "not_found" {
		t.Fatal(err)
	}
	h.service.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err = h.service.Download(ctx, "alice", v.ID, name); Code(err) != "artifact_expired" {
		t.Fatal(err)
	}
}
func TestInstallationReplacementAndRecordCapacityAreExplicit(t *testing.T) {
	h := setup(t)
	h.cfg.MaxRecords = 1
	h.service.cfg.MaxRecords = 1
	_ = h.submit("one")
	if _, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "two", DatasetID: "tiny-v1"}); Code(err) != "record_capacity" {
		t.Fatal(err)
	}
	p, err := h.client.Resource(PVCs).Namespace(h.cfg.Namespace).Get(ctx, "batch-artifacts", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.SetUID("another-pvc")
	_, _ = h.client.Resource(PVCs).Namespace(h.cfg.Namespace).Update(ctx, p, metav1.UpdateOptions{})
	if err = h.service.Ready(ctx); Code(err) != "installation_changed" {
		t.Fatal(err)
	}
}

func TestKnownRejectedSubmissionIsRetriedOnlyByExplicitCaller(t *testing.T) {
	h := setup(t)
	var rejected atomic.Bool
	h.client.PrependReactor("create", "jobs", func(a kt.Action) (bool, runtime.Object, error) {
		if !rejected.Swap(true) {
			return true, nil, apierrors.NewForbidden(Jobs.GroupResource(), "job", fmt.Errorf("quota exceeded"))
		}
		return false, nil, nil
	})
	request := Request{RequestID: "reject", DatasetID: "tiny-v1"}
	if _, _, err := h.service.Submit(ctx, "alice", request); Code(err) != "submission_rejected" {
		t.Fatal(err)
	}
	if err := h.service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if h.created.Load() != 0 {
		t.Fatal("background replayed a rejected submission")
	}
	v, _, err := h.service.Submit(ctx, "alice", request)
	if err != nil || v.ID == "" || v.Reason != "" || h.created.Load() != 1 {
		t.Fatal(v, err)
	}
}

func TestRetentionCleanupKeepsIdempotencyRecord(t *testing.T) {
	h := setup(t)
	v := h.submit("expire")
	h.complete(v.ID, false)
	v, err := h.service.Get(ctx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.service.Cancel(ctx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Delete(ctx, "pod-success", metav1.DeleteOptions{})
	h.service.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err = h.service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = h.service.store.root.Stat("published/" + strings.Split(v.ID, ".")[0]); !os.IsNotExist(err) {
		t.Fatal("expired data retained", err)
	}
	replayed, created, err := h.service.Submit(ctx, "alice", Request{RequestID: "expire", DatasetID: "tiny-v1"})
	if err != nil || created || replayed.ID != v.ID || h.created.Load() != 1 {
		t.Fatal(replayed, created, err)
	}
}

func TestStoreCatalogIgnoresActiveAtomicWritesAndReclaimsOrphanTemps(t *testing.T) {
	h := setup(t)
	v := h.submit("staging")
	name := strings.Split(v.ID, ".")[0]
	unlock, err := h.service.store.lock(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	path := "records/" + name + ".json." + strings.Repeat("a", 32) + ".tmp"
	if err = h.service.store.root.WriteFile(path, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	names, err := h.service.store.names()
	if err != nil || len(names) != 1 {
		t.Fatal(names, err)
	}
	if _, err = h.service.store.root.Stat(path); err != nil {
		t.Fatal("active temporary was removed", err)
	}
	unlock()
	names, err = h.service.store.names()
	if err != nil || len(names) != 1 {
		t.Fatal(names, err)
	}
	if _, err = h.service.store.root.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan temp remains", err)
	}
}

func TestManifestDigestAcceptsImportedReferenceAndRejectsTagOnlyIdentity(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			h := setup(t)
			v := h.submit("digest")
			h.complete(v.ID, false)
			p, err := h.client.Resource(Pods).Namespace(h.cfg.Namespace).Get(ctx, "pod-success", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
			status := statuses[0].(map[string]interface{})
			status["image"] = h.cfg.WorkerImageID
			if valid {
				status["imageID"] = "docker.io/library/import-date@" + h.cfg.WorkerImageDigest
			} else {
				status["imageID"] = "docker.io/library/import-date@sha256:" + strings.Repeat("f", 64)
			}
			_ = unstructured.SetNestedSlice(p.Object, statuses, "status", "containerStatuses")
			_, _ = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Update(ctx, p, metav1.UpdateOptions{})
			got, err := h.service.Get(ctx, "alice", v.ID)
			if valid {
				if err != nil || got.State != "succeeded" {
					t.Fatal(got, err)
				}
			} else if Code(err) != "image_identity_changed" {
				t.Fatal("accepted wrong immutable image identity", err)
			}
		})
	}
}

func TestUnknownReadsAndCapacityRejectedWritesDoNotAllocateLocks(t *testing.T) {
	h := setup(t)
	h.service.cfg.MaxRecords = 1
	_ = h.submit("full")
	count := func() int {
		f, err := h.service.store.root.Open("locks")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		entries, err := f.ReadDir(-1)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for i := 0; i < 16; i++ {
		unknown := "bat-" + hash([]byte(fmt.Sprint(i)))[:40] + ".missing"
		if _, err := h.service.Get(ctx, "alice", unknown); Code(err) != "not_found" {
			t.Fatal(err)
		}
		if _, _, err := h.service.Submit(ctx, "alice", Request{RequestID: fmt.Sprintf("overflow-%d", i), DatasetID: "tiny-v1"}); Code(err) != "record_capacity" {
			t.Fatal(err)
		}
	}
	if got := count(); got != before {
		t.Fatalf("unbounded lock allocation: before=%d after=%d", before, got)
	}
}

func TestCancelledSubmissionRemovesUnpublishedStagingData(t *testing.T) {
	h := setup(t)
	v := h.submit("partial")
	r := h.record(v.ID)
	path := "published/" + r.Name + ".pending"
	if err := h.service.store.root.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.service.store.root.WriteFile(path+"/partial.png", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Cancel(ctx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.store.root.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unpublished staging data survived cancellation", err)
	}
}
