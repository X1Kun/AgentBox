package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/X1Kun/AgentBox/internal/admission"
	"github.com/X1Kun/AgentBox/internal/admission/health"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log/slog"
	"net"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func address(value string) bool {
	host, _, err := net.SplitHostPort(value)
	return err == nil && net.ParseIP(host) != nil
}
func run(args []string) error {
	flags := flag.NewFlagSet("agentbox-admission", flag.ContinueOnError)
	var config, kubeconfig, leaseNamespace, metricsListen, probeListen string
	flags.StringVar(&config, "config", "", "explicit policy YAML file; changes require a new health revision")
	flags.StringVar(&kubeconfig, "kubeconfig", "", "explicit scoped kubeconfig, or in-cluster credentials")
	flags.StringVar(&leaseNamespace, "lease-namespace", "", "namespace for the controller's dedicated Lease")
	flags.StringVar(&metricsListen, "metrics-listen", "127.0.0.1:19103", "restricted metrics listener")
	flags.StringVar(&probeListen, "probe-listen", "127.0.0.1:19104", "health/readiness listener")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || config == "" || !address(metricsListen) || !address(probeListen) || leaseNamespace == "" {
		return errors.New("explicit policy, lease namespace and IP listeners required")
	}
	cfg, err := admission.ReadConfig(config)
	if err != nil {
		return err
	}
	var restConfig *rest.Config
	if kubeconfig == "" {
		restConfig, err = rest.InClusterConfig()
	} else {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return errors.New("unable to load admission credentials")
	}
	restConfig.Timeout = time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	restConfig.QPS, restConfig.Burst = 10, 20
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return errors.New("unable to configure admission client")
	}
	factory := func(c admission.Config) (health.Source, error) {
		return health.NewPrometheus(c.PrometheusURL, c.CAFile, c.Health, time.Duration(c.RequestTimeoutSeconds)*time.Second)
	}
	source, err := factory(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := source.(interface{ Close() }); ok {
			closer.Close()
		}
	}()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	backend, err := admission.New(dynamicClient, cfg, source, logger)
	if err != nil {
		return err
	}
	backend.SetReload(func() (admission.Config, error) { return admission.ReadConfig(config) }, factory)
	backend.SetLeaderNamespace(leaseNamespace)
	backend.SetMetrics(admission.NewMetrics(metrics.Registry))
	workload, check := admission.WorkloadObject(), admission.CheckObject()
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		LeaderElection: true, LeaderElectionResourceLock: "leases", LeaderElectionID: cfg.Check, LeaderElectionNamespace: leaseNamespace,
		LeaderElectionReleaseOnCancel: false, LeaderElectionLabels: map[string]string{"agentbox.dev/session": cfg.Owner},
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}, ByObject: map[client.Object]cache.ByObject{
			workload: {}, check: {Field: fields.OneTermEqualSelector(metav1.ObjectNameField, cfg.Check)},
		}},
		Metrics: metricsserver.Options{BindAddress: metricsListen}, HealthProbeBindAddress: probeListen,
	})
	if err != nil {
		return errors.New("unable to start admission manager")
	}
	if err = manager.AddHealthzCheck("alive", healthz.Ping); err != nil {
		return err
	}
	if err = backend.Setup(manager); err != nil {
		return err
	}
	logger.Info("admission manager starting", "namespace", cfg.Namespace, "check", cfg.Check, "revision", cfg.Health.Revision)
	if err = manager.Start(ctrl.SetupSignalHandler()); err != nil {
		return errors.New("admission manager stopped")
	}
	return nil
}
