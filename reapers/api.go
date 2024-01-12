package reapers

import (
	"context"

	"github.com/cilium/cilium/api/v1/models"
	nomad_api "github.com/hashicorp/nomad/api"
)

type AllocationInfo interface {
	Info(allocID string, q *nomad_api.QueryOptions) (*nomad_api.Allocation, *nomad_api.QueryMeta, error)
}

type EventStreamer interface {
	Stream(ctx context.Context, topics map[nomad_api.Topic][]string, index uint64, q *nomad_api.QueryOptions) (<-chan *nomad_api.Events, error)
}

type EndpointLister interface {
	EndpointList() ([]*models.Endpoint, error)
}

type EndpointGetter interface {
	EndpointGet(id string) (*models.Endpoint, error)
}

type EndpointPatcher interface {
	EndpointPatch(id string, ep *models.EndpointChangeRequest) error
}

type EndpointUpdater interface {
	EndpointLister
	EndpointGetter
	EndpointPatcher
}

type PolicyGetter interface {
	PolicyGet(labels []string) (*models.Policy, error)
}

type PolicyReplacer interface {
	PolicyReplace(policyJSON string, replace bool, replaceWithLabels []string) (*models.Policy, error)
}

type PolicyDeleter interface {
	PolicyDelete(labels []string) (*models.Policy, error)
}

type PolicyUpdater interface {
	PolicyGetter
	PolicyReplacer
	PolicyDeleter
}
