package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/urfave/cli/v2"
	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
)

type config struct {
	apiServer string
	logLevel  int
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
			Usage:       "URL of the Kubernetes API server (not required if --k8s-kubeconfig is given) (default: http://localhost:8443)",
			Destination: &cliCfg.apiServer,
		},
		&cli.IntFlag{
			Name:        "loglevel",
			Usage:       "log verbosity and level: info, warn, fatal, error are always printed no matter the log level. Use 5 for debug (default: 4)",
			Destination: &cliCfg.logLevel,
			Value:       4,
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
		// TODO: Metrics, if we want metrics we need to use a unique pod since the pod will be host networked
		MetricsBindAddress: "0",
	})

	if err != nil {
		return err
	}
	err = ctrl.
		NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(csrapprover.Predicate)).
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
