package reapers

import (
	"reflect"
	"testing"

	"github.com/cilium/cilium/api/v1/models"
	endpoint_id "github.com/cilium/cilium/pkg/endpoint/id"
	"github.com/cosmonic-labs/netreap/internal/netreap"
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
				endpointPatchFn: func(id string, ep *models.EndpointChangeRequest) error {
					expectedID := endpoint_id.NewCiliumID(endpointOne.ID)
					expectedContainerID := endpointOne.Status.ExternalIdentifiers.ContainerID

					if id != expectedID {
						t.Errorf("wrong endpoint ID passed, expected %v, got %v", expectedID, id)
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
			reaper, err := NewEndpointReaper(tt.cilium, tt.nomadAllocations, nil, "", netreap.IgnoredLabels{})
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
