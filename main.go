package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	cilium_client "github.com/cilium/cilium/pkg/client"
	cilium_command "github.com/cilium/cilium/pkg/command"
	cilium_kvstore "github.com/cilium/cilium/pkg/kvstore"
	cilium_logging "github.com/cilium/cilium/pkg/logging"
	nomad_api "github.com/hashicorp/nomad/api"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"

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
			if m, err := cilium_command.ToStringMapStringE(kvStoreOpt); err != nil {
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
	cilium_logging.DefaultLogger.SetReportCaller(true)
	cilium_logging.DefaultLogger.SetOutput(io.Discard)
	cilium_logging.DefaultLogger.AddHook(zaplogrus.NewZapLogrusHook(logger))

	return logger, nil
}

func run(ctx context.Context, conf config) error {

	logger, err := configureLogging(conf.debug)
	if err != nil {
		return fmt.Errorf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	// Step 0: Construct the clients

	// Looks for the default Cilium socket path or uses the value from CILIUM_SOCK
	cilium_client, err := cilium_client.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("error when connecting to cilium agent: %s", err)
	}

	// Fetch kvstore config from Cilium if not set
	if conf.kvStore == "" || len(conf.kvStoreOpts) == 0 {
		resp, err := cilium_client.ConfigGet()
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

	err = cilium_kvstore.Setup(ctx, conf.kvStore, conf.kvStoreOpts, nil)
	if err != nil {
		return fmt.Errorf("unable to connect to Cilium kvstore: %s", err)
	}

	// DefaultConfig fetches configuration data from well-known nomad variables (e.g. NOMAD_ADDR,
	// NOMAD_CACERT), so we'll just leverage that for now.
	nomad_client, err := nomad_api.NewClient(nomad_api.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Nomad: %s", err)
	}

	// Get the node ID of the instance we're running on
	self, err := nomad_client.Agent().Self()
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

	// Step 1: Leader election
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	zap.L().Debug("Starting node reaper")
	node_reaper, err := reapers.NewNodeReaper(ctx, cilium_kvstore.Client(), nomad_client.Nodes(), nomad_client.EventStream(), os.Getenv("NOMAD_ALLOC_ID"))
	if err != nil {
		return err
	}

	nodeFailChan, err := node_reaper.Run()
	if err != nil {
		return fmt.Errorf("unable to start node reaper: %s", err)
	}

	// Step 2: Start the reapers
	zap.L().Debug("Starting endpoint reaper")
	endpoint_reaper, err := reapers.NewEndpointReaper(cilium_client, nomad_client.Allocations(), nomad_client.EventStream(), nodeID)
	if err != nil {
		return err
	}
	endpointFailChan, err := endpoint_reaper.Run(ctx)

	zap.S().Debug("Starting policies reaper")
	policies_reaper, err := reapers.NewPoliciesReaper(cilium_kvstore.Client(), conf.policiesPrefix, cilium_client)
	if err != nil {
		return err
	}
	policiesFailChan, err := policies_reaper.Run(ctx)

	// Wait for interrupt or client failure
	select {
	case <-c:
		zap.S().Info("Received interrupt, shutting down")
		cancel()
	case <-nodeFailChan:
		zap.S().Error("nomad node reaper client failed, shutting down")
		cancel()
	case <-endpointFailChan:
		zap.S().Error("endpoint reaper kvstore client failed, shutting down")
		cancel()
	case <-policiesFailChan:
		zap.S().Error("policies reaper kvstore client failed, shutting down")
		cancel()
	}

	return nil
}
