// Package batch adapts fixed image Jobs and independently retained artifacts.
package batch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var Jobs = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
var Pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var Workloads = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
var Queues = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "localqueues"}
var PVCs = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

const InstanceKey = "agentbox.dev/batch-instance"
const PrincipalKey = "agentbox.dev/batch-principal"
const RequestKey = "agentbox.dev/batch-request-sha256"
const TemplateKey = "agentbox.dev/batch-template-sha256"
const MaxFileBytes = 256 << 10
const MaxOutputBytes = 2 << 20

var dns = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var uid = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
var requestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var taskID = regexp.MustCompile(`^(bat-[a-f0-9]{40})\.([a-z0-9-]{1,64})$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{code} }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "backend_unavailable"
}
func hash(data []byte) string               { v := sha256.Sum256(data); return hex.EncodeToString(v[:]) }
func principalHash(principal string) string { return hash([]byte(principal))[:32] }

type Request struct {
	RequestID string `json:"request_id"`
	DatasetID string `json:"dataset_id"`
	Passes    int    `json:"passes,omitempty"`
}
type File struct {
	Name        string `json:"name"`
	Size        int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	PixelSHA256 string `json:"pixel_sha256"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}
type Contract struct {
	Version       int    `json:"schema_version"`
	DatasetID     string `json:"dataset_id"`
	DatasetSHA256 string `json:"dataset_sha256"`
	Outputs       []File `json:"outputs"`
}
type View struct {
	ID                string `json:"batch_id"`
	State             string `json:"state"`
	Reason            string `json:"reason,omitempty"`
	CleanupComplete   bool   `json:"cleanup_complete"`
	ArtifactExpiresAt string `json:"artifact_expires_at,omitempty"`
	Artifacts         []File `json:"artifacts,omitempty"`
}
type Record struct {
	Version   int     `json:"schema_version"`
	Name      string  `json:"name"`
	Principal string  `json:"principal"`
	Request   Request `json:"request"`
	Digest    string  `json:"request_sha256"`
	Template  string  `json:"template_sha256"`
	JobUID    string  `json:"job_uid"`
	Attempted bool    `json:"create_attempted"`
	State     string  `json:"state"`
	Reason    string  `json:"reason,omitempty"`
	Cancel    bool    `json:"cancel_requested"`
	Release   bool    `json:"release_requested"`
	ExpiresAt string  `json:"artifact_expires_at,omitempty"`
	Artifacts []File  `json:"artifacts,omitempty"`
	PodUID    string  `json:"pod_uid,omitempty"`
}
type Config struct {
	Namespace, Instance, DataDir, TemplateFile, ContractFile, WorkerImageID, WorkerImageDigest string
	Retention                                                                                  time.Duration
	MaxRecords                                                                                 int
}
type Service struct {
	client         dynamic.Interface
	cfg            Config
	template       *batchv1.Job
	templateHash   string
	contract       Contract
	store          *Store
	now            func() time.Time
	reconcileMu    sync.Mutex
	reconcileAfter string
}
type Backend interface {
	Submit(context.Context, string, Request) (View, bool, error)
	Get(context.Context, string, string) (View, error)
	Cancel(context.Context, string, string) (View, error)
	Download(context.Context, string, string, string) ([]byte, error)
	Ready(context.Context) error
	Reconcile(context.Context) error
}
