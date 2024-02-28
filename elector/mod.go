package elector

import (
	"context"
	"time"

	ciliumKvStore "github.com/cilium/cilium/pkg/kvstore"

	"go.uber.org/zap"
)

const keyName = "service/netreaper/leader"

// Elector is a helper type for performing and managing leader election
type Elector struct {
	ctx          context.Context
	client       ciliumKvStore.BackendOperations
	lock         ciliumKvStore.KVLocker
	lockAcquired chan struct{}
	clientID     []byte
}

// New returns a fully initialized Elector that can be used to obtain leader election
func New(ctx context.Context, client ciliumKvStore.BackendOperations, clientID string) (*Elector, error) {
	elector := Elector{
		ctx:          ctx,
		client:       client,
		lockAcquired: make(chan struct{}),
		clientID:     []byte(clientID),
	}

	go elector.pollLock()

	return &elector, nil
}

// SeizeThrone attempts to attain a leader lock among multiple instances. The returned channel
// return a value if this instance was elected
func (e *Elector) SeizeThrone() <-chan struct{} {
	return e.lockAcquired
}

// StepDown gracefully steps down as the leader (retiring to the country side to till the earth like
// a virtuous Roman citizen). This should be called with `defer` once the Elector is
// created so that it can step down cleanly
func (e *Elector) StepDown() {
	zap.L().Debug("Attempting to release leader lock")

	// If this is called during normal cleanup, we don't need to release
	if !e.IsLeader() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := e.lock.Unlock(ctx)
	if err != nil {
		// Just log an error if this happens and default to false
		zap.L().Warn("Unable to run release lock", zap.Error(err))
		return
	}

	e.lock = nil
}

// IsLeader returns true if this node is currently the leader
func (e *Elector) IsLeader() bool {
	return e.lock != nil
}

func (e *Elector) acquire() bool {
	zap.L().Debug("Attempting to acquire leader lock")

	lock, err := e.client.LockPath(e.ctx, keyName)
	if err != nil {

		// Cilium has a Hint() function that turns useful errors in to strings for some cases
		// So we have to check for the string to be able to ignore timeouts for logging purposes
		if err.Error() != "etcd client timeout exceeded" {
			// Just log an error if this happens and default to false
			zap.L().Error("Unable to run acquire against kvstore", zap.Error(err))
		}

		return false
	}

	e.lock = lock

	modified, err := e.client.UpdateIfDifferentIfLocked(e.ctx, keyName, e.clientID, false, e.lock)
	if err != nil {
		zap.L().Warn("Unable to update leader key with allocID", zap.Error(err))
	}

	if !modified {
		zap.L().Warn("Was already leader?", zap.Error(err))
	}

	return true
}

func (e *Elector) pollLock() {
	// When first called, try to get a lock. If we don't, start waiting
	if e.acquire() {
		e.lockAcquired <- struct{}{}
		// We're leader, so no need to wait
		return
	}

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	for {
		select {

		case <-e.ctx.Done():
			zap.L().Info("Received shutdown signal, stopping lock acquisition loop")
			return

		case <-tick.C:
			if e.acquire() {
				e.lockAcquired <- struct{}{}
				// We're leader, so no need to wait
				return
			}
			zap.L().Debug("Unable to acquire leader lock")
		}
	}
}
