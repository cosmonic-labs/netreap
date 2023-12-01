package policy

import (
	"context"
	"fmt"

	"github.com/cilium/cilium/api/v1/client/policy"
	ciliumApi "github.com/cilium/cilium/pkg/api"
	ciliumClient "github.com/cilium/cilium/pkg/client"
	consulApi "github.com/hashicorp/consul/api"
	consulWatch "github.com/hashicorp/consul/api/watch"
	"github.com/hashicorp/go-hclog"
	"go.uber.org/zap"
)

const (
	PolicyKeyDefault = "netreap.io/policy"
)

type Poller struct {
	consulClient *consulApi.Client
	ciliumClient *ciliumClient.Client
	watchKey     string
}

func NewPoller(client *consulApi.Client, key string) (*Poller, error) {
	cilium, err := ciliumClient.NewDefaultClient()
	if err != nil {
		return nil, err
	}

	return &Poller{
		consulClient: client,
		ciliumClient: cilium,
		watchKey:     key,
	}, nil
}

func (p *Poller) Run(ctx context.Context) error {
	l := zap.NewStdLog(zap.L().Named("policy_poller"))
	log := zap.S().Named("policy_poller")
	log.Info(fmt.Sprintf("starting Consul watch for key: %s", p.watchKey))

	params := map[string]interface{}{
		"type": "key",
		"key":  p.watchKey,
	}

	wp, err := consulWatch.Parse(params)
	if err != nil {
		return fmt.Errorf("unable to create watch plan: %w", err)
	}

	lastIndex := uint64(0)
	wp.HybridHandler = func(blockVal consulWatch.BlockingParamVal, val interface{}) {
		idxVal, ok := blockVal.(consulWatch.WaitIndexVal)
		if !ok {
			log.Errorf("invalid parameter for blocking: expected an index-based watch")
			return
		}

		idx := uint64(idxVal)
		if idx != lastIndex {
			lastIndex = idx
			pair, ok := val.(*consulApi.KVPair)
			if !ok {
				log.Error("unable to retrieve value: was not a KVPair")
				return
			}

			getParams := policy.NewGetPolicyParams().WithContext(ctx).WithTimeout(ciliumApi.ClientTimeout)
			oldPolicy, err := p.ciliumClient.Policy.GetPolicy(getParams)
			if err != nil {
				log.Error(fmt.Sprintf("failed to get policy: %s", err.Error()))
				return
			}

			deleteParams := policy.NewDeletePolicyParams().WithContext(ctx).WithTimeout(ciliumApi.ClientTimeout)
			_, err = p.ciliumClient.Policy.DeletePolicy(deleteParams)
			if err != nil {
				log.Error(fmt.Sprintf("failed to delete policy: %s", err.Error()))
				return
			}

			policyParams := policy.NewPutPolicyParams().WithPolicy(string(pair.Value)).WithTimeout(ciliumApi.ClientTimeout).WithContext(ctx)
			_, err = p.ciliumClient.Policy.PutPolicy(policyParams)
			if err != nil {
				log.Error(fmt.Sprintf("unable to put Cilium policy: %s", err.Error()))
				// try to put old policy back
				policyParams = policyParams.WithPolicy(oldPolicy.Payload.Policy)
				_, err = p.ciliumClient.Policy.PutPolicy(policyParams)
				if err != nil {
					log.Errorf("failed to put old policy back: %s", err)
				}
				return
			}
		}

		log.Info("loaded new policy")
	}

	go func() {
		hclogger := hclog.FromStandardLogger(l, &hclog.LoggerOptions{
			Name: "policy_poller",
		})
		err := wp.RunWithClientAndHclog(p.consulClient, hclogger)
		if err != nil {
			zap.S().Error("error running watcher", zap.Error(err))
		}
	}()

	go func() {
		<-ctx.Done()
		wp.Stop()
	}()

	return nil
}
