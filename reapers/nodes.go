package reapers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/node/types"
	"github.com/cosmonic-labs/netreap/elector"
	"github.com/hashicorp/nomad/api"
	"go.uber.org/zap"
)

const nodePrefix = "cilium/state/nodes/v1/default/"

type NodeReaper struct {
	allocID          string
	kvStoreClient    kvstore.BackendOperations
	nomadNodeInfo    NodeInfo
	nomadEventStream EventStreamer
}

// NewNodeReaper creates a new NodeReaper. This will run an initial reconciliation before returning the
// reaper
func NewNodeReaper(kvStoreClient kvstore.BackendOperations, nomadNodeInfo NodeInfo, nomadEventStream EventStreamer, allocID string) (*NodeReaper, error) {
	reaper := NodeReaper{
		allocID:          allocID,
		kvStoreClient:    kvStoreClient,
		nomadNodeInfo:    nomadNodeInfo,
		nomadEventStream: nomadEventStream,
	}

	return &reaper, nil
}

// Run the reaper until the context given in the contructor is cancelled. This function is non
// blocking and will only return errors if something occurs during startup
// return a channel to notify of nomad client failure
func (n *NodeReaper) Run(ctx context.Context) error {

	// Do the initial reconciliation loop
	if err := n.reconcile(ctx); err != nil {
		return fmt.Errorf("unable to perform initial reconciliation: %s", err)
	}

	// NOTE: Specifying uint max so that it starts from the next available index. If there is a
	// better way to start from latest index, we can change this
	queryOptions := &api.QueryOptions{Namespace: "*"}
	eventChan, err := n.nomadEventStream.Stream(
		ctx,
		map[api.Topic][]string{
			api.TopicNode: {"*"},
		},
		math.MaxInt64,
		queryOptions.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("error when starting node event stream: %s", err)
	}

	// Leader election
	election, err := elector.New(ctx, n.kvStoreClient, n.allocID)
	if err != nil {
		zap.L().Error("Unable to set up leader election for node reaper", zap.Error(err))
		return err
	}
	zap.L().Info("Waiting for leader election")
	<-election.SeizeThrone()
	zap.L().Info("Elected as leader, starting node reaping")
	defer election.StepDown()

	go startKvstoreWatchdog()

	tick := time.NewTicker(time.Hour)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			zap.L().Info("Context cancelled, shutting down node reaper")
			return ctx.Err()

		case <-tick.C:
			zap.L().Info("Reconciliation loop started")
			if err := n.reconcile(ctx); err != nil {
				zap.L().Error("Error occurred during reconcilation, will retry next loop", zap.Error(err))
			}

		case events := <-eventChan:
			if events.Err != nil {
				zap.L().Error("Got error message from node event channel", zap.Error(events.Err))
				return events.Err
			}

			if events.IsHeartbeat() {
				continue
			}

			zap.L().Debug("Got events from Node topic. Handling...", zap.Int("event-count", len(events.Events)))

			for _, evt := range events.Events {
				if evt.Type != "NodeDeregistration" {
					continue
				}

				zap.L().Debug("Got node deregistration event, deleting node from KV store")

				raw, ok := evt.Payload["Node"]
				if !ok {
					zap.L().Warn("NodeDeregistration event didn't contain a Node payload", zap.Any("event", evt))
					continue
				}

				node, ok := raw.(api.Node)
				if !ok {
					zap.S().Errorf("Node payload wasn't of type Node. Got type %T", raw)
					continue
				}

				err := n.kvStoreClient.Delete(ctx, nodePrefix+node.Name)
				if err != nil {
					zap.L().Error("Unable to delete node from kvstore. Will retry on next reconciliation", zap.String("node-name", node.Name), zap.Error(err))
				}
			}
		}
	}
}

func (n *NodeReaper) reconcile(ctx context.Context) error {
	zap.L().Debug("Beginning reconciliation")

	zap.L().Debug("Getting nomad node list")
	queryOptions := api.QueryOptions{}
	nodes, _, err := n.nomadNodeInfo.List(queryOptions.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("unable to list nodes: %s", err)
	}

	// Convert nodes to map for easy lookup
	nodeMap := map[string]struct{}{}
	for _, node := range nodes {
		nodeMap[node.Name] = struct{}{}
	}
	zap.L().Debug("Finished constructing list of all nodes", zap.Any("nodes", nodeMap))

	zap.L().Debug("Fetching cilium nodes from kvstore")
	rawNodes, err := n.kvStoreClient.ListPrefix(ctx, nodePrefix)
	if err != nil {
		return fmt.Errorf("unable to list current cilium nodes: %s", err)
	}

	// Loop through all the nodes and remove any that aren't in the kvstore anymore
	for key, value := range rawNodes {

		node := types.Node{}
		if err := json.Unmarshal(value.Data, &node); err != nil {
			return fmt.Errorf("invalid data found when parsing Cilium node: %s", err)
		}

		if _, ok := nodeMap[node.Name]; !ok {
			zap.L().Debug("Node no longer exists in Nomad, deleting", zap.String("node", node.Name))
			// NOTE: This delete only works to cleanup nodes where the node has stopped along with
			// the cilium agent. Otherwise cilium will just recreate this entry
			if err := n.kvStoreClient.Delete(ctx, key); err != nil {
				zap.L().Error("Error when cleaning up node. Will retry on next reconciliation", zap.Error(err))
			}
		}

	}

	return nil
}
