package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"

	cilium_client "github.com/cilium/cilium/pkg/client"
	cilium_logging "github.com/cilium/cilium/pkg/logging"
	consul_api "github.com/hashicorp/consul/api"
	nomad_api "github.com/hashicorp/nomad/api"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"

	"github.com/cosmonic-labs/netreap/internal/netreap"
	"github.com/cosmonic-labs/netreap/internal/policy"
	"github.com/cosmonic-labs/netreap/internal/zaplogrus"
	"github.com/cosmonic-labs/netreap/reapers"
)

var Version = "unreleased"

type config struct {
	policyKey     string
	ignoredLabels string
}

func main() {
	conf := config{}
	var debug bool

	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	app := &cli.App{
		Name:  "netreap",
		Usage: "A custom monitor and reaper for cleaning up Cilium endpoints and nodes",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "debug",
				Value:       false,
				Usage:       "Enable debug logging",
				EnvVars:     []string{"NETREAP_DEBUG"},
				Destination: &debug,
			},
			&cli.StringFlag{
				Name:        "policy-key",
				Aliases:     []string{"k"},
				Value:       policy.PolicyKeyDefault,
				Usage:       "Consul key to watch for Cilium policy updates.",
				EnvVars:     []string{"NETREAP_POLICY_KEY"},
				Destination: &conf.policyKey,
			},
			&cli.StringFlag{
				Name:        "ignore-nomad-meta-labels",
				Aliases:     []string{"i"},
				Value:       "",
				Usage:       "List of ignored labels collected from Nomad's job metainfo.",
				EnvVars:     []string{"NETREAP_IGNORED_META_LABELS"},
				Destination: &conf.ignoredLabels,
			},
		},
		Before: func(ctx *cli.Context) error {
			if debug {
				devlog, err := zap.NewDevelopment()
				if err != nil {
					return err
				}
				logger = devlog
			}
			zap.ReplaceGlobals(logger)

			// Bridge Cilium logrus to netreap zap
			cilium_logging.DefaultLogger.SetReportCaller(true)
			cilium_logging.DefaultLogger.SetOutput(io.Discard)
			cilium_logging.DefaultLogger.AddHook(zaplogrus.NewZapLogrusHook(logger))

			return nil
		},
		Action: func(c *cli.Context) error {
			return run(conf)
		},
		Version: Version,
	}

	if err := app.Run(os.Args); err != nil {
		zap.S().Fatal(err)
	}
}

func run(conf config) error {
	// Step 0: Construct clients
	consul_client, err := consul_api.NewClient(consul_api.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Consul: %s", err)
	}

	// Looks for the default Cilium socket path or uses the value from CILIUM_SOCK
	cilium_client, err := cilium_client.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("error when connecting to cilium agent: %s", err)
	}

	// DefaultConfig fetches configuration data from well-known nomad variables (e.g. NOMAD_ADDR,
	// NOMAD_CACERT), so we'll just leverage that for now.
	nomad_client, err := nomad_api.NewClient(nomad_api.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Nomad: %s", err)
	}

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

	zap.S().Debug("Starting node reaper")
	node_reaper, err := reapers.NewNodeReaper(ctx, nomad_client, consul_client)
	if err != nil {
		return err
	}

	nodeFailChan, err := node_reaper.Run()
	if err != nil {
		return fmt.Errorf("unable to start node reaper: %s", err)
	}

	nomadIgnoredLables := netreap.IgnoredLabels{IgnoreMeta: strings.Split(conf.ignoredLabels, ",")}
	zap.S().Debug("Starting endpoint reaper")
	endpoint_reaper, err := reapers.NewEndpointReaper(cilium_client, nomad_client.Allocations(), nomad_client.EventStream(), nodeID, nomadIgnoredLables)
	if err != nil {
		return err
	}

	endpointFailChan, err := endpoint_reaper.Run(ctx)
	if err != nil {
		return fmt.Errorf("unable to start endpoint reaper: %s", err)
	}

	zap.S().Debug("starting policy poller")
	poller, err := policy.NewPoller(consul_client, conf.policyKey)
	if err != nil {
		return err
	}

	if err := poller.Run(ctx); err != nil {
		return fmt.Errorf("unable to start policy poller: %w", err)
	}

	// Wait for interrupt or client failure
	select {
	case <-c:
		zap.S().Info("Received interrupt, shutting down")
		cancel()
	case <-nodeFailChan:
		zap.S().Error("nomad node reaper client failed, shutting down")
		cancel()
	case <-endpointFailChan:
		zap.S().Error("endpoint reaper consul client failed, shutting down")
		cancel()
	}

	return nil
}
