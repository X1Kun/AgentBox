// Package admission implements one opt-in, single-domain Kueue AdmissionCheck.
package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/admission/health"
	yaml "go.yaml.in/yaml/v3"
	"io"
	"k8s.io/apimachinery/pkg/util/validation"
	"os"
	"time"
)

const ControllerName = "agentbox.dev/health-admission"
const RevisionAnnotation = "agentbox.dev/policy-revision"

type Config struct {
	Version               int             `json:"schema_version"`
	Namespace             string          `json:"namespace"`
	LocalQueue            string          `json:"local_queue"`
	ClusterQueue          string          `json:"cluster_queue"`
	Check                 string          `json:"admission_check"`
	Owner                 string          `json:"owner"`
	Health                health.Contract `json:"health"`
	PrometheusURL         string          `json:"prometheus_url"`
	CAFile                string          `json:"ca_file,omitempty"`
	MinimumSamples        int64           `json:"minimum_samples"`
	HealthyRatio          float64         `json:"healthy_ratio"`
	ProtectRatio          float64         `json:"protect_ratio"`
	MaxAgeSeconds         int             `json:"max_age_seconds"`
	StableSeconds         int             `json:"stable_seconds"`
	ProtectSeconds        int             `json:"protect_seconds"`
	PermitIntervalSeconds int             `json:"permit_interval_seconds"`
	MaxWaitSeconds        int             `json:"max_wait_seconds"`
	RetrySeconds          int             `json:"retry_seconds"`
	PollSeconds           int             `json:"poll_seconds"`
	RequestTimeoutSeconds int             `json:"request_timeout_seconds"`
}

func (c Config) Validate() error {
	for _, v := range []string{c.Namespace, c.LocalQueue, c.ClusterQueue, c.Check} {
		if len(validation.IsDNS1123Subdomain(v)) != 0 || len(v) > 63 {
			return errors.New("invalid admission scope")
		}
	}
	if c.Version != 1 || !health.ValidName(c.Owner) || !c.Health.Valid() || c.PrometheusURL == "" || c.MinimumSamples < 1 || c.MinimumSamples > 2000 || !(c.ProtectRatio >= 0 && c.ProtectRatio < c.HealthyRatio && c.HealthyRatio <= 1) {
		return errors.New("invalid admission policy")
	}
	for _, v := range []int{c.MaxAgeSeconds, c.StableSeconds, c.ProtectSeconds, c.PermitIntervalSeconds, c.MaxWaitSeconds, c.RetrySeconds, c.PollSeconds, c.RequestTimeoutSeconds} {
		if v < 1 || v > 3600 {
			return errors.New("invalid admission budget")
		}
	}
	if c.PollSeconds > 30 || c.RequestTimeoutSeconds > 10 || c.MaxAgeSeconds < c.Health.WindowSeconds+c.Health.DeadlineSeconds || c.MaxWaitSeconds < c.RetrySeconds {
		return errors.New("inconsistent admission budgets")
	}
	return nil
}
func ReadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("config_unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return Config{}, errors.New("config_invalid")
	}
	y := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if y.Decode(&node) != nil || y.Decode(new(yaml.Node)) != io.EOF || !safeNode(&node) {
		return Config{}, errors.New("config_invalid")
	}
	var value any
	if node.Decode(&value) != nil {
		return Config{}, errors.New("config_invalid")
	}
	data, err = json.Marshal(value)
	if err != nil {
		return Config{}, errors.New("config_invalid")
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("config_invalid")
	}
	return c, c.Validate()
}

func safeNode(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode {
		return false
	}
	if node.Kind != yaml.DocumentNode {
		allowed := false
		for _, tag := range []string{"!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null"} {
			if node.Tag == tag {
				allowed = true
			}
		}
		if !allowed {
			return false
		}
	}
	for _, child := range node.Content {
		if !safeNode(child) {
			return false
		}
	}
	return true
}
func (c Config) fingerprint() string {
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func seconds(v int) time.Duration { return time.Duration(v) * time.Second }
func (c Config) sameScope(other Config) bool {
	return c.Namespace == other.Namespace && c.LocalQueue == other.LocalQueue && c.ClusterQueue == other.ClusterQueue && c.Check == other.Check && c.Owner == other.Owner
}
