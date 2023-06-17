// Copyright 2021 Matthew Holt

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// 	http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyrl

import (
	"bytes"
	"context"
	"encoding/gob"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// DistributedRateLimiting enables and customizes distributed rate limiting.
// It works by writing out the state of all internal rate limiters to storage,
// and reading in the state of all other rate limiters in the cluster, every
// so often.
//
// Distributed rate limiting is not exact like the standard internal rate
// limiting, but it is eventually consistent. Lower (more frequent) sync
// intervals will result in higher consistency and precision, but more I/O
// and CPU overhead.
type DistributedRateLimiting struct {
	// How often to sync internal state to storage. Default: 5s
	WriteInterval caddy.Duration `json:"write_interval,omitempty"`

	// How often to sync other instances' states from storage.
	// Default: 5s
	ReadInterval caddy.Duration `json:"read_interval,omitempty"`

	instanceID string

	otherStates   []rlState
	otherStatesMu sync.RWMutex
}

func (h Handler) syncDistributed(ctx context.Context) {
	readTicker := time.NewTicker(time.Duration(h.Distributed.ReadInterval))
	writeTicker := time.NewTicker(time.Duration(h.Distributed.WriteInterval))
	defer readTicker.Stop()
	defer writeTicker.Stop()

	for {
		select {
		case <-readTicker.C:
			// get all the latest stored rate limiter states
			err := h.syncDistributedRead(ctx)
			if err != nil {
				h.logger.Error("syncing distributed limiter states", zap.Error(err))
			}

		case <-writeTicker.C:
			// store all current rate limiter states
			err := h.syncDistributedWrite(ctx)
			if err != nil {
				h.logger.Error("distributing internal state", zap.Error(err))
			}

		case <-ctx.Done():
			return
		}
	}
}

// syncDistributedWrite stores all rate limiter states.
func (h Handler) syncDistributedWrite(ctx context.Context) error {
	state := rlState{
		Timestamp: now(),
		Zones:     make(map[string]map[string]int),
	}

	// iterate all rate limit zones
	rateLimits.Range(func(zoneName, value interface{}) bool {
		zoneNameStr := zoneName.(string)
		zoneLimiters := value.(*sync.Map)

		state.Zones[zoneNameStr] = make(map[string]int)

		// iterate all limiters within zone
		zoneLimiters.Range(func(key, value interface{}) bool {
			if value == nil {
				return true
			}
			rl := value.(*ringBufferRateLimiter)

			eventsInWindow := rl.Count(state.Timestamp)
			state.Zones[zoneNameStr][key.(string)] = eventsInWindow

			return true
		})

		return true
	})

	buf := gobBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer gobBufPool.Put(buf)

	err := gob.NewEncoder(buf).Encode(state)
	if err != nil {
		return err
	}
	err = h.storage.Store(ctx, path.Join(storagePrefix, h.Distributed.instanceID+".rlstate"), buf.Bytes())
	if err != nil {
		return err
	}

	return nil
}

// syncDistributedRead loads all rate limiter states from other instances.
func (h Handler) syncDistributedRead(ctx context.Context) error {
	instanceFiles, err := h.storage.List(ctx, storagePrefix, false)
	if err != nil {
		return err
	}
	if len(instanceFiles) == 0 {
		return nil
	}

	otherStates := make([]rlState, 0, len(instanceFiles)-1)

	for _, instanceFile := range instanceFiles {
		// skip our own file
		if strings.HasSuffix(instanceFile, h.Distributed.instanceID+".rlstate") {
			continue
		}

		encoded, err := h.storage.Load(ctx, instanceFile)
		if err != nil {
			h.logger.Error("unable to load distributed rate limiter state",
				zap.String("key", instanceFile),
				zap.Error(err))
			continue
		}

		var state rlState
		err = gob.NewDecoder(bytes.NewReader(encoded)).Decode(&state)
		if err != nil {
			h.logger.Error("corrupted rate limiter state file",
				zap.String("key", instanceFile),
				zap.Error(err))
			continue
		}

		otherStates = append(otherStates, state)
	}

	h.Distributed.otherStatesMu.Lock()
	h.Distributed.otherStates = otherStates
	h.Distributed.otherStatesMu.Unlock()

	return nil
}

// distributedRateLimiting enforces limiter (keyed by rlKey) in consideration of all other instances in the cluster.
// If the limit is exceeded, the response is prepared and the relevant error is returned. Otherwise, a reservation
// is made in the local limiter and no error is returned.
func (h Handler) distributedRateLimiting(w http.ResponseWriter, repl *caddy.Replacer, limiter *ringBufferRateLimiter, rlKey, zoneName string) error {
	maxAllowed := limiter.MaxEvents()
	window := limiter.Window()

	var totalCount int

	h.Distributed.otherStatesMu.RLock()
	defer h.Distributed.otherStatesMu.RUnlock()

	for _, otherInstanceState := range h.Distributed.otherStates {
		// if instance hasn't reported in longer than the window, no point in counting with it
		if otherInstanceState.Timestamp.Before(now().Add(-window)) {
			continue
		}

		// if instance has this zone, add last known limiter count
		if zone, ok := otherInstanceState.Zones[zoneName]; ok {
			// TODO: could probably skew the numbers here based on timestamp and window... perhaps try to predict a better updated count
			totalCount += zone[rlKey]

			// no point in counting more if we're already over
			if totalCount >= maxAllowed {
				return h.rateLimitExceeded(w, repl, zoneName, 0) // TODO: how do we calculate a good wait duration?
			}
		}
	}

	// add our own internal count (we do this at the end instead of the beginning
	// so the critical section over this limiter's lock is smaller), and make the
	// reservation if we're within the limit
	limiter.mu.Lock()
	totalCount += limiter.countUnsynced(now())
	if totalCount < maxAllowed {
		limiter.reserve()
		limiter.mu.Unlock()
		return nil
	}
	limiter.mu.Unlock()

	// otherwise, it appears limit has been exceeded
	return h.rateLimitExceeded(w, repl, zoneName, 0) // TODO: how do we calculate a good wait duration?
}

type rlState struct {
	// When these values were recorded.
	Timestamp time.Time

	// Map of zone name to map of all rate limiters in that
	// zone by key to the number of events within window.
	Zones map[string]map[string]int
}

var gobBufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

const storagePrefix = "rate_limit/instances"
