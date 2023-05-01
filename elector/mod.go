package elector

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"go.uber.org/zap"
)

const keyName = "service/netreaper/leader"
const ttl = "30s"

// Elector is a helper type for performing and managing leader election
type Elector struct {
	ctx           context.Context
	client        *api.Client
	sessionID     string
	isLeader      bool
	stopWatchFunc func()
	lockAcquired  chan struct{}
	tryAgain      chan struct{}
}

// New returns a fully initialized Elector that can be used to obtain leader election
func New(ctx context.Context, client *api.Client) (*Elector, error) {
	sessionID, _, err := client.Session().Create(&api.SessionEntry{Name: "netreap", TTL: ttl}, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to start leader election: %s", err)
	}

	elector := Elector{
		ctx:          ctx,
		client:       client,
		sessionID:    sessionID,
		isLeader:     false,
		lockAcquired: make(chan struct{}),
		tryAgain:     make(chan struct{}),
	}

	go elector.autoRenewSession()
	go elector.pollLock()
	go elector.startKeyWatch()
	return &elector, nil
}

// SeizeThrone attempts to attain a leader lock among multiple instances. The returned channel
// return a value if this instance was elected
func (e *Elector) SeizeThrone() <-chan struct{} {
	return e.lockAcquired
}

// StepDown gracefully steps down as the leader (retiring to the country side to till the earth like
// a virtuous Roman citizen). This should generally be called with `defer` once the Elector is
// created so that it can step down
func (e *Elector) StepDown() {
	// If this is called during normal cleanup, we don't need to release
	if !e.isLeader {
		return
	}

	acquired, _, err := e.client.KV().Release(&api.KVPair{
		Key:     keyName,
		Value:   []byte{},
		Session: e.sessionID,
	}, nil)
	if err != nil || !acquired {
		// Just log an error if this happens and default to false
		zap.S().Warnf("Unable to run release against Consul: %s", err)
		return
	}
	e.isLeader = false
	// Theoretically, someone could call this function outside of cleanup, so let's just be safe and
	// restart the pollLock function
	go e.pollLock()
}

// IsLeader returns true if this node is currently the leader
func (e *Elector) IsLeader() bool {
	return e.isLeader
}

func (e *Elector) acquire() bool {
	acquired, _, err := e.client.KV().Acquire(&api.KVPair{
		Key: keyName,
		// NOTE: If we want to, we can add actual data here
		Value:   []byte{},
		Session: e.sessionID,
	}, nil)
	if err != nil {
		// Just log an error if this happens and default to false
		zap.S().Errorf("Unable to run acquire against Consul: %s", err)
		return false
	}
	return acquired
}

func (e *Elector) acquireRetry(num_retries uint) bool {
	// Wait to try again until the timer goes off. Per the docs for leader election, there should be
	// a timed wait before retries.

	// First thing, just try to acquire
	if e.acquire() {
		return true
	}
	zap.S().Debugf("Unable to acquire lock. Retrying up to %d times", num_retries)
	for i := 0; i < int(num_retries); i++ {
		timer := time.NewTimer(10 * time.Second)
		<-timer.C
		if e.acquire() {
			return true
		}
		zap.S().Debugf("Lock retry %d did not succeed", i+1)
	}
	zap.S().Debug("Never acquired lock after retry")
	return false
}

func (e *Elector) pollLock() {
	// When first called, try to get a lock. If we don't, start waiting
	if e.acquire() {
		e.lockAcquired <- struct{}{}
		e.isLeader = true
		// We're leader, so no need to wait
		return
	}
	for {
		select {
		case <-e.ctx.Done():
			zap.S().Info("Received shutdown signal, stopping lock acquisition loop")
			return
		case <-e.tryAgain:
			if e.acquireRetry(6) {
				e.lockAcquired <- struct{}{}
				e.isLeader = true
				// We're leader, so no need to wait
				return
			}
		}
	}
}

func (e *Elector) autoRenewSession() {
	go e.client.Session().RenewPeriodic(ttl, e.sessionID, nil, e.ctx.Done())
}

func (e *Elector) startKeyWatch() error {
	params := map[string]interface{}{
		"type": "key",
		"key":  keyName,
	}
	plan, err := watch.Parse(params)
	if err != nil {
		panic(fmt.Errorf("the watch plan should compile, this is programmer error: %s", err))
	}

	plan.Handler = func(index uint64, data interface{}) {
		if data != nil {
			pair, ok := data.(*api.KVPair)
			if !ok {
				zap.S().Errorf("Unable to parse data as KVPair, got type %T", data)
				return
			}
			if pair.Session == "" {
				e.tryAgain <- struct{}{}
			}
		}
	}

	e.stopWatchFunc = plan.Stop

	return plan.RunWithClientAndHclog(e.client, nil)
}
