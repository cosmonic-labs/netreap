package reapers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	endpoint_id "github.com/cilium/cilium/pkg/endpoint/id"
	nomad_api "github.com/hashicorp/nomad/api"
)

type allocationInfoMock struct {
	infoFn func(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error)
}

func (p *allocationInfoMock) Info(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error) {
	if p != nil && p.infoFn != nil {
		return p.infoFn(allocID, q)
	}
	return nil, nil, nil
}

type eventStreamerMock struct {
	streamFn func(ctx context.Context, topics map[nomad_api.Topic][]string, index uint64, q *nomad_api.QueryOptions) (<-chan *nomad_api.Events, error)
}

func (p *eventStreamerMock) Stream(ctx context.Context, topics map[nomad_api.Topic][]string, index uint64, q *nomad_api.QueryOptions) (<-chan *nomad_api.Events, error) {
	if p != nil && p.streamFn != nil {
		return p.streamFn(ctx, topics, index, q)
	}
	return nil, nil
}

type endpointUpdaterMock struct {
	endpointListFn  func() ([]*models.Endpoint, error)
	endpointGetFn   func(id string) (*models.Endpoint, error)
	endpointPatchFn func(id string, ep *models.EndpointChangeRequest) error
}

func (p *endpointUpdaterMock) EndpointList() ([]*models.Endpoint, error) {
	if p != nil && p.endpointListFn != nil {
		return p.endpointListFn()
	}
	return nil, nil
}

func (p *endpointUpdaterMock) EndpointGet(id string) (*models.Endpoint, error) {
	if p != nil && p.endpointGetFn != nil {
		return p.endpointGetFn(id)
	}
	return nil, nil
}

func (p *endpointUpdaterMock) EndpointPatch(id string, ep *models.EndpointChangeRequest) error {
	if p != nil && p.endpointPatchFn != nil {
		return p.endpointPatchFn(id, ep)
	}
	return nil
}

func TestEndpointReconcile(t *testing.T) {
	endpointOne := &models.Endpoint{
		ID: 1,
		Status: &models.EndpointStatus{
			ExternalIdentifiers: &models.EndpointIdentifiers{
				ContainerID: "containerID",
			},
			Labels: &models.LabelConfigurationStatus{
				SecurityRelevant: models.Labels{"reserved:init"},
			},
		},
	}
	allocationOne := &nomad_api.Allocation{
		ID:        "containerID",
		JobID:     "jobID",
		Namespace: "namespace",
		TaskGroup: "taskGroup",
		Job: &nomad_api.Job{
			Meta: map[string]string{},
		},
	}
	endpointOneLabels := models.Labels{
		"netreap:nomad.job_id=jobID",
		"netreap:nomad.namespace=namespace",
		"netreap:nomad.task_group_id=taskGroup",
	}

	tests := []struct {
		name             string
		cilium           *endpointUpdaterMock
		nomadAllocations *allocationInfoMock
		shouldErr        bool
	}{
		{
			"No Endpoints",
			&endpointUpdaterMock{
				endpointListFn: func() ([]*models.Endpoint, error) {
					return []*models.Endpoint{}, nil
				},
				endpointPatchFn: func(id string, ep *models.EndpointChangeRequest) error {
					t.Fatalf("unexpected call to patch endpoint")
					return nil
				},
			},
			&allocationInfoMock{
				infoFn: func(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error) {
					t.Fatalf("unexpected call to allocation info")
					return nil, nil, nil
				},
			},
			false,
		},
		{
			"One endpoint",
			&endpointUpdaterMock{
				endpointListFn: func() ([]*models.Endpoint, error) {
					return []*models.Endpoint{endpointOne}, nil
				},
				endpointPatchFn: func(endpointID string, ep *models.EndpointChangeRequest) error {
					expectedID := endpoint_id.NewCiliumID(endpointOne.ID)
					expectedContainerID := endpointOne.Status.ExternalIdentifiers.ContainerID

					if endpointID != expectedID {
						t.Errorf("wrong endpoint ID passed, expected %v, got %v", expectedID, endpointID)
					}

					if ep.ContainerID != expectedContainerID {
						t.Errorf("wrong container ID passed, expected %v, got %v", expectedContainerID, ep.ContainerID)
					}

					if !reflect.DeepEqual(ep.Labels, endpointOneLabels) {
						t.Errorf("wrong labels, expected %v, got %v", endpointOneLabels, ep.Labels)
					}

					return nil
				},
			},
			&allocationInfoMock{
				infoFn: func(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error) {
					expectedContainerID := endpointOne.Status.ExternalIdentifiers.ContainerID
					if allocID != expectedContainerID {
						t.Errorf("wrong container ID passed, expected %v, got %v", expectedContainerID, allocID)
					}
					return allocationOne, nil, nil
				},
			},
			false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			reaper, err := NewEndpointReaper(tt.cilium, tt.nomadAllocations, nil, "")
			if err != nil {
				t.Fatalf("unexpected error creating poller %v", err)
			}

			err = reaper.reconcile()

			if tt.shouldErr && err == nil {
				t.Error("expected error but got <nil>")
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("unexpected error %v", err)
			}
		})
	}
}

func TestEndpointRunErrorHandling(t *testing.T) {
	cilium := &endpointUpdaterMock{
		endpointListFn: func() ([]*models.Endpoint, error) {
			return []*models.Endpoint{}, nil
		},
		endpointPatchFn: func(id string, ep *models.EndpointChangeRequest) error {
			t.Fatalf("unexpected call to patch endpoint")
			return nil
		},
	}

	nomad := &allocationInfoMock{
		infoFn: func(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error) {
			t.Fatalf("unexpected call to allocation info")
			return nil, nil, nil
		},
	}

	events := make(chan *nomad_api.Events, 4)

	nomadEventStream := &eventStreamerMock{
		streamFn: func(ctx context.Context, topics map[nomad_api.Topic][]string, index uint64, q *nomad_api.QueryOptions) (<-chan *nomad_api.Events, error) {

			// Should ignore this event and continue
			events <- &nomad_api.Events{
				Index: 1,
				Err: &json.SyntaxError{
					Offset: 0,
				},
			}

			// One normal event
			events <- &nomad_api.Events{
				Index: 2,
				Err:   nil,
				Events: []nomad_api.Event{
					{
						Topic: nomad_api.TopicAllocation,
						Type:  "AllocationUpdated",
					},
				},
			}

			// Should exit at this point with the returned error
			events <- &nomad_api.Events{
				Index: 3,
				Err:   fmt.Errorf("fatal error"),
			}

			// This event will not be consumed as the routine should exit
			events <- &nomad_api.Events{
				Index: 4,
				Err:   nil,
				Events: []nomad_api.Event{
					{
						Topic: nomad_api.TopicAllocation,
						Type:  "AllocationUpdated",
					},
				},
			}

			return events, nil
		},
	}

	reaper, err := NewEndpointReaper(cilium, nomad, nomadEventStream, "NodeID")
	if err != nil {
		t.Fatalf("unexpected error creating poller %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	failChan, err := reaper.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error running endpoint reaper %v", err)
	}

	event := <-events
	if event == nil {
		t.Fatalf("expected left over event but got <nil>")
	}

	fail := <-failChan
	if !fail {
		t.Fatalf("expected fail but got <false>")
	}

	close(events)

}
