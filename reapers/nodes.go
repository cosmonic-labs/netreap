package reapers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/cilium/cilium/pkg/node/types"
	consul_api "github.com/hashicorp/consul/api"
	nomad_api "github.com/hashicorp/nomad/api"
	"go.uber.org/zap"

	"github.com/cosmonic/netreap/elector"
)

const nodePrefix = "cilium/state/nodes/v1/default/"

type NodeReaper struct {
	nomad  *nomad_api.Client
	consul *consul_api.Client
	ctx    context.Context
}

// NewNodeReaper creates a new NodeReaper. This will run an initial reconciliation before returning the
// reaper
func NewNodeReaper(ctx context.Context, nomad_client *nomad_api.Client, consul_client *consul_api.Client) (*NodeReaper, error) {
	reaper := NodeReaper{nomad: nomad_client, consul: consul_client, ctx: ctx}
	// Do the initial reconciliation loop
	if err := reaper.reconcile(); err != nil {
		return nil, fmt.Errorf("unable to perform initial reconciliation: %s", err)
	}
	return &reaper, nil
}

// Run the reaper until the context given in the contructor is cancelled. This function is non
// blocking and will only return errors if something occurs during startup
// return a channel to notify of nomad client failure
func (n *NodeReaper) Run() (<-chan bool, error) {
	// NOTE: Specifying uint max so that it starts from the next available index. If there is a
	// better way to start from latest index, we can change this
	eventChan, err := n.nomad.EventStream().Stream(n.ctx, map[nomad_api.Topic][]string{nomad_api.TopicNode: {}}, math.MaxInt64, nil)
	if err != nil {
		return nil, fmt.Errorf("error when starting node event stream: %s", err)
	}
	failChan := make(chan bool, 1)

	go func() {
		// Leader election
		election, err := elector.New(n.ctx, n.consul)
		if err != nil {
			zap.S().Errorw("Unable to set up leader election for node reaper", "error", err)
			return
		}
		zap.S().Info("Waiting for leader election")
		<-election.SeizeThrone()
		zap.S().Info("Elected as leader, starting node reaping")
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		defer election.StepDown()

		for {
			select {
			case <-n.ctx.Done():
				zap.S().Info("Context cancelled, shutting down node reaper")
				return
			case <-tick.C:
				zap.S().Info("Reconciliation loop started")
				if err := n.reconcile(); err != nil {
					zap.S().Errorf("Error occurred during reconcilation, will retry next loop: %s", err)
				}
			case events := <-eventChan:
				if events.Err != nil {
					zap.S().Errorf("Got error message from node event channel: %s", events.Err)
					failChan <- true
					return
				}
				for _, evt := range events.Events {
					if evt.Type != "NodeDeregistration" {
						continue
					}
					zap.S().Debug("Got node deregistration event, deleting node from KV store")
					raw, ok := evt.Payload["Node"]
					if !ok {
						zap.S().Warnf("NodeDeregistration event didn't contain a Node payload: %v", evt)
						continue
					}
					node, ok := raw.(nomad_api.Node)
					if !ok {
						zap.S().Errorf("Node payload wasn't of type Node. Got type %T", raw)
						continue
					}
					_, err := n.consul.KV().Delete(nodePrefix+node.Name, nil)
					if err != nil {
						zap.S().Errorf("Unable to delete node %s from consul. Will retry on next reconciliation: %s", node.Name, err)
					}
				}
			}
		}
	}()

	return failChan, nil
}

func (n *NodeReaper) reconcile() error {
	zap.S().Debug("Beginning reconciliation")
	zap.S().Debug("Getting nomad node list")
	nodes, _, err := n.nomad.Nodes().List(nil)
	if err != nil {
		return fmt.Errorf("unable to list nodes: %s", err)
	}
	// Convert nodes to map for easy lookup

	nodeMap := map[string]struct{}{}
	for _, node := range nodes {
		nodeMap[node.Name] = struct{}{}
	}
	zap.S().Debugw("Finished constructing list of all nodes", "nodes", nodeMap)
	kv := n.consul.KV()
	zap.S().Debug("Fetching cilium nodes from consul")
	rawNodes, _, err := kv.List(nodePrefix, nil)
	if err != nil {
		return fmt.Errorf("unable to list current cilium nodes: %s", err)
	}

	// Loop through all the nodes in the keystore and remove any that aren't in consul anymore
	for _, pair := range rawNodes {
		node := types.Node{}
		if err := json.Unmarshal(pair.Value, &node); err != nil {
			return fmt.Errorf("invalid data found when parsing Cilium node: %s", err)
		}
		if _, ok := nodeMap[node.Name]; !ok {
			zap.S().Debugw("Node no longer exists in nomad, deleting", "node", node.Name)
			// NOTE: This delete only works to cleanup nodes where the node has stopped along with
			// the cilium agent. Otherwise cilium will just recreate this entry
			if _, err := kv.Delete(pair.Key, nil); err != nil {
				zap.S().Errorf("Error when cleaning up node. Will retry on next reconciliation: %s", err)
			}
		}
	}

	return nil
}
