package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"

	consul_api "github.com/hashicorp/consul/api"
	nomad_api "github.com/hashicorp/nomad/api"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"

	"github.com/cosmonic/netreap/internal/policy"
	"github.com/cosmonic/netreap/reapers"
)

var Version = "unreleased"

type config struct {
	net         string
	policyKey   string
	excludeTags []string
}

func main() {
	conf := config{}
	var debug bool

	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	excludeTags := cli.StringSlice{}

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
				Name:        "cilium-cidr",
				Aliases:     []string{"c"},
				Usage:       "The CIDR block that Cilium addresses belong to. This is used for checking if a service is a Cilium service or not",
				EnvVars:     []string{"NETREAP_CILIUM_CIDR"},
				Destination: &conf.net,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "policy-key",
				Aliases:     []string{"k"},
				Value:       policy.PolicyKeyDefault,
				Usage:       "Consul key to watch for Cilium policy updates.",
				EnvVars:     []string{"NETREAP_POLICY_KEY"},
				Destination: &conf.policyKey,
			},
			&cli.StringSliceFlag{
				Name:        "exclude-tag",
				Aliases:     []string{"e"},
				Usage:       "Consul service tags to skip when checking for Cilium-enabled jobs",
				EnvVars:     []string{"NETREAP_EXCLUDE_TAG"},
				Destination: &excludeTags,
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
			return nil
		},
		Action: func(c *cli.Context) error {
			conf.excludeTags = excludeTags.Value()
			return run(conf)
		},
		Version: Version,
	}

	if err := app.Run(os.Args); err != nil {
		zap.S().Fatal(err)
	}
}

func run(conf config) error {
	_, net, err := net.ParseCIDR(conf.net)
	if err != nil {
		return fmt.Errorf("unable to parse Cilium CIDR block: %s", err)
	}

	// Step 0: Construct clients
	consul_client, err := consul_api.NewClient(consul_api.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Consul: %s", err)
	}

	// DefaultConfig fetches configuration data from well-known nomad variables (e.g. NOMAD_ADDR,
	// NOMAD_CACERT), so we'll just leverage that for now.
	nomad_client, err := nomad_api.NewClient(nomad_api.DefaultConfig())
	if err != nil {
		return fmt.Errorf("unable to connect to Nomad: %s", err)
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

	zap.S().Debug("Starting endpoint reaper")
	endpoint_reaper, err := reapers.NewEndpointReaper(ctx, nomad_client, consul_client, net, conf.excludeTags)
	if err != nil {
		return err
	}

	endpointFailChan, err := endpoint_reaper.Run()
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
