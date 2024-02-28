package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	ciliumClient "github.com/cilium/cilium/pkg/client"
	ciliumCommand "github.com/cilium/cilium/pkg/command"
	ciliumKvStore "github.com/cilium/cilium/pkg/kvstore"
	ciliumLogging "github.com/cilium/cilium/pkg/logging"
	nomadApi "github.com/hashicorp/nomad/api"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/cosmonic-labs/netreap/internal/zaplogrus"
	"github.com/cosmonic-labs/netreap/reapers"
)

var Version = "unreleased"

type config struct {
	debug          bool
	kvStore        string
	kvStoreOpts    map[string]string
	policiesPrefix string
}

func main() {
	ctx := context.Background()

	conf := config{}
	app := &cli.App{
		Name:  "netreap",
		Usage: "A custom monitor and reaper for cleaning up Cilium endpoints and nodes",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "debug",
				Value:       false,
				Usage:       "Enable debug logging",
				EnvVars:     []string{"NETREAP_DEBUG"},
				Destination: &conf.debug,
			},
			&cli.StringFlag{
				Name:        "policies-prefix",
				Aliases:     []string{"p"},
				Value:       reapers.PoliciesKeyPrefix,
				Usage:       "kvstore key prefix to watch for Cilium policy updates.",
				EnvVars:     []string{"NETREAP_POLICIES_PREFIX"},
				Destination: &conf.policiesPrefix,
			},
			&cli.StringFlag{
				Name:        "kvstore",
				Usage:       "Consul key to watch for Cilium policy updates.",
				EnvVars:     []string{"NETREAP_KVSTORE"},
				Destination: &conf.kvStore,
			},
			&cli.StringFlag{
				Name:    "kvstore-opts",
				Usage:   "Consul key to watch for Cilium policy updates.",
				EnvVars: []string{"NETREAP_KVSTORE_OPTS"},
			},
		},
		Before: func(ctx *cli.Context) error {
			// Borrow the parser from Cilium
			kvStoreOpt := ctx.String("kvstore-opts")
			if m, err := ciliumCommand.ToStringMapStringE(kvStoreOpt); err != nil {
				return fmt.Errorf("unable to parse %s: %w", kvStoreOpt, err)
			} else {
				conf.kvStoreOpts = m
			}

			return nil
		},
		Action: func(c *cli.Context) error {
			return run(c.Context, conf)
		},
		Version: Version,
	}

	if err := app.RunContext(ctx, os.Args); err != nil {
		zap.L().Fatal("Error running netreap", zap.Error(err))
	}
}

func configureLogging(debug bool) (logger *zap.Logger, err error) {
	// Step 0: Setup logging

	if debug {
		logger, err = zap.NewDevelopment()
	} else {
		logger, err = zap.NewProduction()
	}

	if err != nil {
		return nil, err
	}

	zap.ReplaceGlobals(logger)

	// Bridge Cilium logrus to netreap zap
	ciliumLogging.DefaultLogger.SetReportCaller(true)
	ciliumLogging.DefaultLogger.SetOutput(io.Discard)
	ciliumLogging.DefaultLogger.AddHook(zaplogrus.NewZapLogrusHook(logger))

	return logger, nil
}

func run(ctx context.Context, conf config) error {

	// Nomad with Docker defaults to SIGTERM for stopping containersq
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger, err := configureLogging(conf.debug)
	if err != nil {
		return fmt.Errorf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	// Step 0: Construct the clients

	// Looks for the default Cilium socket path or uses the value from CILIUM_SOCK
	ciliumClient, err := ciliumClient.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("error when connecting to cilium agent: %s", err)
	}

	// Fetch kvstore config from Cilium if not set
	if conf.kvStore == "" || len(conf.kvStoreOpts) == 0 {
		resp, err := ciliumClient.ConfigGet()
		if err != nil {
			return fmt.Errorf("unable to retrieve cilium configuration: %s", err)
		}
		if resp.Status == nil {
			return fmt.Errorf("unable to retrieve cilium configuration: empty response")
		}

		cfgStatus := resp.Status

		if conf.kvStore == "" {
			conf.kvStore = cfgStatus.KvstoreConfiguration.Type
		}

		if len(conf.kvStoreOpts) == 0 {
			for k, v := range cfgStatus.KvstoreConfiguration.Options {
				conf.kvStoreOpts[k] = v
			}
		}
	}

	err = ciliumKvStore.Setup(ctx, conf.kvStore, conf.kvStoreOpts, nil)
	if err != nil {
		return fmt.Errorf("unable to connect to Cilium kvstore: %s", err)
	}

	// DefaultConfig fetches configuration data from well-known nomad variables (e.g. NOMAD_ADDR,
	// NOMAD_CACERT), so we'll just leverage that for now.
	nomadClient, err := nomadApi.NewClient(nomadApi.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Nomad: %s", err)
	}

	// Get the node ID of the instance we're running on
	self, err := nomadClient.Agent().Self()
	if err != nil {
		return fmt.Errorf("unable to query local agent info: %s", err)
	}

	clientStats, ok := self.Stats["client"]
	if !ok {
		return fmt.Errorf("not running on a client node")
	}

	nodeID, ok := clientStats["node_id"]
	if !ok {
		return fmt.Errorf("unable to get local node ID")
	}

	// Step 2: Start the reapers
	egroup, ctx := errgroup.WithContext(ctx)

	zap.S().Debug("Starting endpoint reaper")
	endpointReaper, err := reapers.NewEndpointReaper(ciliumClient, nomadClient.Allocations(), nomadClient.EventStream(), nodeID)
	if err != nil {
		return err
	}
	egroup.Go(func() error {
		return endpointReaper.Run(ctx)
	})

	zap.S().Debug("Starting policy reaper")
	policiesReaper, err := reapers.NewPoliciesReaper(ciliumKvStore.Client(), conf.policiesPrefix, ciliumClient)
	if err != nil {
		return err
	}
	egroup.Go(func() error {
		return policiesReaper.Run(ctx)
	})

	zap.S().Debug("Starting node reaper")
	nodeReaper, err := reapers.NewNodeReaper(ciliumKvStore.Client(), nomadClient.Nodes(), nomadClient.EventStream(), os.Getenv("NOMAD_ALLOC_ID"))
	if err != nil {
		return err
	}
	egroup.Go(func() error {
		return nodeReaper.Run(ctx)
	})

	// Step 4: Wait interrupt or go routine error
	return egroup.Wait()
}
