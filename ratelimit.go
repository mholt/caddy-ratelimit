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
	"fmt"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// RateLimit describes an HTTP rate limit zone.
type RateLimit struct {
	// Request matchers, which defines the class of requests that are in the RL zone.
	MatcherSetsRaw caddyhttp.RawMatcherSets `json:"match,omitempty" caddy:"namespace=http.matchers"`

	// The key which uniquely differentiates rate limits within this zone. It could
	// be a static string (no placeholders), resulting in one and only one rate limiter
	// for the whole zone. Or, placeholders could be used to dynamically allocate
	// rate limiters. For example, a key of "foo" will create exactly one rate limiter
	// for all clients. But a key of "{http.request.remote.host}" will create one rate
	// limiter for each different client IP address.
	Key string `json:"key,omitempty"`

	// Number of events allowed within the window.
	MaxEvents int `json:"max_events,omitempty"`

	// Duration of the sliding window.
	Window caddy.Duration `json:"window,omitempty"`

	matcherSets caddyhttp.MatcherSets

	zoneName string

	limitersMap *rateLimitersMap
}

func (rl *RateLimit) provision(ctx caddy.Context, name string) error {
	if rl.Window <= 0 {
		return fmt.Errorf("window must be greater than zero")
	}
	if rl.MaxEvents < 0 {
		return fmt.Errorf("max_events must be at least zero")
	}

	if len(rl.MatcherSetsRaw) > 0 {
		matcherSets, err := ctx.LoadModule(rl, "MatcherSetsRaw")
		if err != nil {
			return err
		}
		err = rl.matcherSets.FromInterface(matcherSets)
		if err != nil {
			return err
		}
	}

	// ensure rate limiter state endures across config changes
	rl.limitersMap = newRateLimiterMap()
	if val, loaded := rateLimits.LoadOrStore(name, rl.limitersMap); loaded {
		rl.limitersMap = val.(*rateLimitersMap)
	}
	rl.limitersMap.updateAll(rl.MaxEvents, time.Duration(rl.Window))

	return nil
}

func (rl *RateLimit) permissiveness() float64 {
	return float64(rl.MaxEvents) / float64(rl.Window)
}

type rateLimitersMap struct {
	limiters   map[string]*ringBufferRateLimiter
	limitersMu sync.Mutex
}

func newRateLimiterMap() *rateLimitersMap {
	var rlm rateLimitersMap
	rlm.limiters = make(map[string]*ringBufferRateLimiter)
	return &rlm
}

// getOrInsert returns an existing rate limiter from the map, or inserts a new
// one with the desired settings and returns it.
func (rlm *rateLimitersMap) getOrInsert(key string, maxEvents int, window time.Duration) *ringBufferRateLimiter {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	rateLimiter, ok := rlm.limiters[key]
	if !ok {
		newRateLimiter := newRingBufferRateLimiter(maxEvents, window)
		rlm.limiters[key] = newRateLimiter
		return newRateLimiter
	}
	return rateLimiter
}

// updateAll updates existing rate limiters with new settings.
func (rlm *rateLimitersMap) updateAll(maxEvents int, window time.Duration) {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	for _, limiter := range rlm.limiters {
		limiter.SetMaxEvents(maxEvents)
		limiter.SetWindow(time.Duration(window))
	}
}

// sweep cleans up expired rate limit states.
func (rlm *rateLimitersMap) sweep() {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	for key, rl := range rlm.limiters {
		func(rl *ringBufferRateLimiter) {
			rl.mu.Lock()
			defer rl.mu.Unlock()

			// no point in keeping a ring buffer of size 0 around
			if len(rl.ring) == 0 {
				delete(rlm.limiters, key)
				return
			}

			// get newest event in ring (should come right before oldest)
			cursorNewest := rl.cursor - 1
			if cursorNewest < 0 {
				cursorNewest = len(rl.ring) - 1
			}
			newest := rl.ring[cursorNewest]
			window := rl.window

			// if newest event in memory is outside the window,
			// the entire ring has expired and can be forgotten
			if newest.Add(window).Before(now()) {
				delete(rlm.limiters, key)
			}
		}(rl)
	}
}

// rlStateForZone returns the state of all rate limiters in the map.
func (rlm *rateLimitersMap) rlStateForZone(timestamp time.Time) map[string]rlStateValue {
	state := make(map[string]rlStateValue)

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	for key, rl := range rlm.limiters {
		count, oldestEvent := rl.Count(timestamp)
		state[key] = rlStateValue{
			Count:       count,
			OldestEvent: oldestEvent,
		}
	}

	return state
}
