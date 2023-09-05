package reapers

import (
	"context"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/hashicorp/nomad/api"
)

type AllocationInfo interface {
	Info(allocID string, q *api.QueryOptions) (*api.Allocation, *api.QueryMeta, error)
}

type EventStreamer interface {
	Stream(ctx context.Context, topics map[api.Topic][]string, index uint64, q *api.QueryOptions) (<-chan *api.Events, error)
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
