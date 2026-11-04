// Package batch adapts fixed image Jobs and independently retained artifacts.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/X1Kun/AgentBox/internal/batchprotocol"
	"github.com/X1Kun/AgentBox/internal/telemetry"
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
const MaxFileBytes = batchprotocol.MaxFileBytes
const MaxOutputBytes = batchprotocol.MaxOutputBytes

var dns = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var uid = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
var requestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var taskID = regexp.MustCompile(`^(bat-[a-f0-9]{40})\.([a-z0-9-]{1,64})$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type State = batchprotocol.State

type Error = batchprotocol.Error

func fail(code string) error                { return &Error{Code: code} }
func Code(err error) string                 { return batchprotocol.Code(err) }
func hash(data []byte) string               { v := sha256.Sum256(data); return hex.EncodeToString(v[:]) }
func principalHash(principal string) string { return hash([]byte(principal))[:32] }

type Request = batchprotocol.Request
type File = batchprotocol.File
type Contract = batchprotocol.Contract
type View = batchprotocol.View
type Record struct {
	Version   int     `json:"schema_version"`
	Name      string  `json:"name"`
	Principal string  `json:"principal"`
	Request   Request `json:"request"`
	Digest    string  `json:"request_sha256"`
	Template  string  `json:"template_sha256"`
	JobUID    string  `json:"job_uid"`
	Attempted bool    `json:"create_attempted"`
	State     State   `json:"state"`
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
	metrics        *telemetry.Metrics
}
