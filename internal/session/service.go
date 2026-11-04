// Package session adapts the upstream Claim lifecycle. Kubernetes remains the source of truth.
package session

import (
	"context"
	"errors"
	sessionprotocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"k8s.io/client-go/dynamic"
	"log/slog"
	"time"
)

type State = sessionprotocol.State

type Error = sessionprotocol.Error

func fail(code string) error { return &Error{Code: code} }

func Code(err error) string { return publicBackendCode(err) }

type Config struct {
	Namespace, Pool, Instance, RuntimeClass string
	DefaultTTL, MaxTTL                      int
	CloseWait                               time.Duration
}

type CreateRequest = sessionprotocol.CreateRequest

type View = sessionprotocol.View

type Service struct {
	client  dynamic.Interface
	cfg     Config
	now     func() time.Time
	runtime Runtime
	logger  *slog.Logger
	metrics *telemetry.Metrics
}

func (s *Service) SetTelemetry(metrics *telemetry.Metrics) { s.metrics = metrics }

func ValidPrincipal(value string) bool { return principalPattern.MatchString(value) }

func New(client dynamic.Interface, cfg Config) (*Service, error) {
	if client == nil || !dns.MatchString(cfg.Namespace) || !dns.MatchString(cfg.Pool) || !dns.MatchString(cfg.Instance) {
		return nil, errors.New("invalid session backend configuration")
	}
	if cfg.RuntimeClass == "" {
		cfg.RuntimeClass = "agentbox-gvisor"
	}
	if !dns.MatchString(cfg.RuntimeClass) {
		return nil, errors.New("invalid runtime class")
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 600
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 3600
	}
	if cfg.DefaultTTL < 60 || cfg.MaxTTL < cfg.DefaultTTL || cfg.MaxTTL > 86400 || cfg.CloseWait < 0 || cfg.CloseWait > 10*time.Second {
		return nil, errors.New("invalid lifecycle budgets")
	}
	return &Service{client: client, cfg: cfg, now: time.Now}, nil
}

func (s *Service) Ready(ctx context.Context) error {
	_, err := s.getResource(ctx, Pools, s.cfg.Pool)
	return err
}
