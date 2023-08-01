package reapers

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	endpoint_id "github.com/cilium/cilium/pkg/endpoint/id"
	nomad_api "github.com/hashicorp/nomad/api"
	"go.uber.org/zap"
)

const (
	netreapLabelPrefix = "netreap"
	nomadLabelPrefix   = "nomad"
	jobIDLabel         = "nomad.job_id"
	taskGroupLabel     = "nomad.task_group_id"
	namespaceLabel     = "nomad.namespace"
)

type EndpointReaper struct {
	cilium           EndpointUpdater
	nomadAllocations AllocationInfo
	nomadEventStream EventStreamer
}

// NewEndpointReaper creates a new EndpointReaper. This will run an initial reconciliation before
// returning the reaper
func NewEndpointReaper(ciliumClient EndpointUpdater, nomadAllocations AllocationInfo, nomadEventStream EventStreamer) (*EndpointReaper, error) {
	reaper := EndpointReaper{
		cilium:           ciliumClient,
		nomadAllocations: nomadAllocations,
		nomadEventStream: nomadEventStream,
	}

	// Do the initial reconciliation loop
	if err := reaper.reconcile(); err != nil {
		return nil, fmt.Errorf("unable to perform initial reconciliation: %s", err)
	}

	return &reaper, nil
}

// Run the reaper until the context given in the contructor is cancelled. This function is non
// blocking and will only return errors if something occurs during startup
// return a channel to notify of consul client failures
func (e *EndpointReaper) Run(ctx context.Context) (<-chan bool, error) {

	// NOTE: Specifying uint max so that it starts from the next available index. If there is a
	// better way to start from latest index, we can change this
	eventChan, err := e.nomadEventStream.Stream(
		ctx,
		map[nomad_api.Topic][]string{
			nomad_api.TopicJob: {},
		},
		math.MaxInt64,
		&nomad_api.QueryOptions{
			Namespace: "*",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error when starting node event stream: %s", err)
	}

	failChan := make(chan bool, 1)

	go func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				zap.L().Info("Context cancelled, shutting down endpoint reaper")
				return

			case <-tick.C:
				zap.L().Info("Periodic reconciliation loop started")
				if err := e.reconcile(); err != nil {
					zap.L().Error("Error occurred during reconcilation, will retry next loop", zap.Error(err))
				}

			case events := <-eventChan:
				if events.Err != nil {
					zap.L().Debug("Got error message from node event channel", zap.Error(events.Err))
					failChan <- true
					return
				}

				zap.L().Debug("Got events from Allocation topic. Handling...", zap.Int("event-count", len(events.Events)))

				for _, event := range events.Events {
					switch event.Type {
					case "AllocationUpdated":
						go e.handleAllocationUpdated(event)
					default:
						zap.L().Debug("Ignoring unhandled event from Allocation topic", zap.String("event-type", event.Type))
						continue
					}
				}
			}
		}
	}()

	return failChan, nil
}

func (e *EndpointReaper) reconcile() error {
	zap.L().Debug("Starting reconciliation")

	// Get current endpoints list
	endpoints, err := e.cilium.EndpointList()
	if err != nil {
		return fmt.Errorf("unable to list current cilium endpoints: %s", err)
	}

	zap.L().Debug("checking each endpoint", zap.Int("endpoints-total", len(endpoints)))

	for _, endpoint := range endpoints {
		endpointID := endpoint_id.NewCiliumID(endpoint.ID)
		containerID := endpoint.Status.ExternalIdentifiers.ContainerID

		// Only managing endpoints with container IDs
		if containerID == "" {
			zap.L().Debug("Skipping endpoint that is not associated with a container",
				zap.String("endpoint-id", endpointID),
			)
			continue
		}

		// Nomad calls the CNI plugin with the allocation ID as the container ID
		allocation, _, err := e.nomadAllocations.Info(containerID, &nomad_api.QueryOptions{Namespace: "*"})
		if err != nil {
			zap.L().Warn("Couldn't fetch allocation from Nomad",
				zap.String("container-id", containerID),
				zap.String("endpoint-id", endpointID),
				zap.Error(err),
			)
			continue
		}

		if allocation != nil {
			zap.L().Debug("Patching labels on endpoint",
				zap.String("container-id", containerID),
				zap.String("endpoint-id", endpointID),
				zap.Error(err),
			)

			labels := e.createLabelsForAllocation(allocation)

			e.labelEndpoint(endpointID, allocation.ID, allocation.Name, labels)
		} else {
			zap.L().Debug("Skipping endpoint as allocation not in Nomad",
				zap.String("container-id", containerID),
				zap.String("endpoint-id", endpointID),
				zap.Error(err),
			)
		}
	}

	zap.L().Debug("Finished reconciliation")

	return nil
}

func (e *EndpointReaper) handleAllocationUpdated(event nomad_api.Event) {
	allocation, err := event.Allocation()
	if err != nil {
		zap.L().Debug("Unable to deserialize Allocation",
			zap.String("event-type", event.Type),
			zap.Uint64("event-index", event.Index),
		)
		return
	}

	if allocation == nil {
		zap.L().Debug("Allocation was empty",
			zap.String("event-type", event.Type),
			zap.Uint64("event-index", event.Index),
		)
		return
	}

	if allocation.NetworkStatus == nil || allocation.NetworkStatus.Address == "" {
		zap.L().Debug("Allocation has no IP address, ignoring",
			zap.String("event-type", event.Type),
			zap.Uint64("event-index", event.Index),
			zap.String("container-id", allocation.ID),
		)
		return
	}

	allocationIP := net.ParseIP(allocation.NetworkStatus.Address)
	endpointID := endpoint_id.NewIPPrefixID(allocationIP)

	endpoint, err := e.cilium.EndpointGet(endpointID)
	if err != nil {
		fields := []zap.Field{zap.String("event-type", event.Type),
			zap.Uint64("event-index", event.Index),
			zap.String("container-id", allocation.ID),
			zap.String("endpoint-id", endpointID),
			zap.Error(err),
		}
		if strings.Contains(err.Error(), "getEndpointIdNotFound") {
			// This is fine, the endpoint probably just isn't on this host
			zap.L().Debug("Endpoint not found", fields...)
		} else {
			zap.L().Warn("Unable to get endpoint", fields...)
		}
		return
	}

	if allocation.Job == nil {
		// Fetch the full allocation since the event didn't have the Job with the metadata
		allocation, _, err = e.nomadAllocations.Info(allocation.ID, &nomad_api.QueryOptions{Namespace: allocation.Namespace})
		if err != nil {
			zap.L().Warn("Couldn't fetch allocation from Nomad",
				zap.String("event-type", event.Type),
				zap.Uint64("event-index", event.Index),
				zap.String("container-id", allocation.ID),
				zap.String("endpoint-id", endpointID),
				zap.Error(err),
			)
			return
		}
	}

	labels := e.createLabelsForAllocation(allocation)

	e.labelEndpoint(endpoint_id.NewCiliumID(endpoint.ID), allocation.ID, allocation.Name, labels)
}

func (e *EndpointReaper) createLabelsForAllocation(allocation *nomad_api.Allocation) models.Labels {
	labels := models.Labels{fmt.Sprintf("%s:%s=%s", netreapLabelPrefix, jobIDLabel, allocation.JobID)}
	labels = append(labels, fmt.Sprintf("%s:%s=%s", netreapLabelPrefix, namespaceLabel, allocation.Namespace))
	labels = append(labels, fmt.Sprintf("%s:%s=%s", netreapLabelPrefix, taskGroupLabel, allocation.TaskGroup))

	// Combine the metadata from the job and the task group with the task group taking precedence
	metadata := make(map[string]string)
	for k, v := range allocation.Job.Meta {
		metadata[k] = v
	}

	for _, taskGroup := range allocation.Job.TaskGroups {
		if *taskGroup.Name == allocation.TaskGroup {
			for k, v := range taskGroup.Meta {
				metadata[k] = v
			}
		}
	}

	for k, v := range metadata {
		labels = append(labels, fmt.Sprintf("%s:%s=%s", nomadLabelPrefix, k, v))
	}

	return labels
}

func (e *EndpointReaper) labelEndpoint(endpointID string, containerID string, containerName string, labels models.Labels) {
	ecr := &models.EndpointChangeRequest{
		ContainerID:   containerID,
		ContainerName: containerName,
		Labels:        labels,
		State:         models.EndpointStateWaitingDashForDashIdentity.Pointer(),
	}
	err := e.cilium.EndpointPatch(endpointID, ecr)
	if err != nil {
		zap.L().Error("Error while patching the endpoint labels of container",
			zap.String("container-id", containerID),
			zap.String("endpoint-id", endpointID),
			zap.Strings("labels", labels),
			zap.Error(err),
		)
	}
}
