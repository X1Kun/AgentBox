package batch

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/dynamic"
	"os"
	"strings"
	"time"
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
	if err = validateTemplate(&job, cfg.Namespace, cfg.WorkerImageDigest); err != nil {
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
