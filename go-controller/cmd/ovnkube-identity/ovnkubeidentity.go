package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovnwebhook"
	"github.com/urfave/cli/v2"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type config struct {
	apiServer          string
	logLevel           int
	port               int
	host               string
	certDir            string
	metricsAddress     string
	enableInterconnect bool
}

var cliCfg config

func main() {
	c := cli.NewApp()
	c.Name = "ovnkube-identity"
	c.Usage = "run ovn-kubernetes identity manager"

	c.Action = func(c *cli.Context) error {
		ctrl.SetLogger(klog.NewKlogr())
		var level klog.Level
		if err := level.Set(strconv.Itoa(cliCfg.logLevel)); err != nil {
			klog.Errorf("Failed to set klog log level %v", err)
			os.Exit(1)
		}

		return run(c)
	}

	c.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "k8s-apiserver",
			Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given)",
			Destination: &cliCfg.apiServer,
		},
		&cli.IntFlag{
			Name:        "loglevel",
			Usage:       "log verbosity and level: info, warn, fatal, error are always printed no matter the log level. Use 5 for debug (default: 4)",
			Destination: &cliCfg.logLevel,
			Value:       4,
		},
		&cli.StringFlag{
			Name:        "webhook-cert-dir",
			Usage:       "directory that contains the server key and certificate",
			Destination: &cliCfg.certDir,
		},
		&cli.StringFlag{
			Name:        "webhook-host",
			Usage:       "the address that the webhook server will listen on",
			Value:       "localhost",
			Destination: &cliCfg.host,
		},
		&cli.IntFlag{
			Name:        "webhook-port",
			Usage:       "port number that the webhook server will serve",
			Value:       webhook.DefaultPort,
			Destination: &cliCfg.port,
		},
		&cli.StringFlag{
			Name:        "metrics-address",
			Usage:       "address that the metrics server will serve",
			Value:       "0",
			Destination: &cliCfg.metricsAddress,
		},
		&cli.BoolFlag{
			Name:        "enable-interconnect",
			Usage:       "Configure to enable interconnecting multiple zones.",
			Destination: &cliCfg.enableInterconnect,
			Value:       false,
		},
	}

	ctx := context.Background()

	// trap SIGHUP, SIGINT, SIGTERM, SIGQUIT and
	// cancel the context
	ctx, cancel := context.WithCancel(ctx)
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer func() {
		signal.Stop(exitCh)
		cancel()
	}()
	go func() {
		select {
		case s := <-exitCh:
			klog.Infof("Received signal %s. Shutting down", s)
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := c.RunContext(ctx, os.Args); err != nil {
		klog.Exit(err)
	}
}

func run(c *cli.Context) error {
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	if cliCfg.apiServer != "" {
		restCfg.Host = cliCfg.apiServer
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    cliCfg.host,
			Port:    cliCfg.port,
			CertDir: cliCfg.certDir,
		}),
		MetricsBindAddress:            cliCfg.metricsAddress,
		LeaderElection:                true,
		LeaderElectionID:              c.App.Name,
		LeaseDuration:                 utilpointer.Duration(time.Minute),
		RenewDeadline:                 utilpointer.Duration(time.Second * 30),
		RetryPeriod:                   utilpointer.Duration(time.Second * 20),
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}

	err = ctrl.NewWebhookManagedBy(mgr).For(&corev1.Node{}).WithValidator(ovnwebhook.NewNodeAdmissionWebhook(cliCfg.enableInterconnect)).Complete()
	if err != nil {
		return fmt.Errorf("failed to setup the node admission webhook: %v", err)
	}

	// in non-ic ovnkube-node does not have the permissions to update pods
	if cliCfg.enableInterconnect {
		err = ctrl.NewWebhookManagedBy(mgr).For(&corev1.Pod{}).WithValidator(ovnwebhook.NewPodAdmissionWebhook(mgr.GetClient())).Complete()
		if err != nil {
			return fmt.Errorf("failed to setup the pod admission webhook: %v", err)
		}
	}

	if err != nil {
		return err
	}
	err = ctrl.
		NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(csrapprover.Predicate)).
		WithOptions(controller.Options{
			// Explicitly enable leader election for CSR approver
			NeedLeaderElection: utilpointer.Bool(true),
			RecoverPanic:       utilpointer.Bool(true),
		}).
		Complete(csrapprover.NewController(
			mgr.GetClient(),
			csrapprover.NamePrefix,
			csrapprover.Organization,
			csrapprover.Groups,
			csrapprover.UserPrefixes,
			csrapprover.Usages,
			csrapprover.MaxDuration,
			mgr.GetEventRecorderFor(csrapprover.ControllerName),
		))
	if err != nil {
		klog.Errorf("Failed to create %s: %v", csrapprover.ControllerName, err)
		os.Exit(1)
	}

	return mgr.Start(c.Context)
}
