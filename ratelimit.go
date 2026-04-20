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

// idleLimiterTTL defines how long a concurrency limiter can remain in memory
// with 0 active requests before it is garbage collected by the sweep() routine.
const idleLimiterTTL = 5 * time.Minute

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

	// Maximum number of concurrent requests allowed.
	MaxConcurrent int `json:"max_concurrent,omitempty"`

	matcherSets caddyhttp.MatcherSets

	zoneName string

	limitersMap *rateLimitersMap
}

func (rl *RateLimit) provision(ctx caddy.Context, name string) error {
	if rl.MaxConcurrent > 0 && (rl.MaxEvents > 0 || rl.Window > 0) {
		return fmt.Errorf("cannot use both max_concurrent and window/max_events")
	}
	if rl.MaxConcurrent == 0 && rl.Window <= 0 {
		return fmt.Errorf("window must be greater than zero")
	}
	if rl.MaxConcurrent == 0 && rl.MaxEvents < 0 {
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
	rl.limitersMap.updateAll(rl)

	return nil
}

func (rl *RateLimit) permissiveness() float64 {
	if rl.MaxConcurrent > 0 {
		// To make concurrency limits comparable to rate limits for sorting,
		// we treat MaxConcurrent as a rate of events per second.
		return float64(rl.MaxConcurrent) / float64(time.Second)
	}
	return float64(rl.MaxEvents) / float64(rl.Window)
}

// RateLimiter is a marker interface for rate limiters.
// It will exclusively be either *ringBufferRateLimiter or *concurrencyLimiter.
type RateLimiter interface {
	isRateLimiter()
}

type rateLimitersMap struct {
	limiters   map[string]RateLimiter
	limitersMu sync.Mutex
}

func newRateLimiterMap() *rateLimitersMap {
	var rlm rateLimitersMap
	rlm.limiters = make(map[string]RateLimiter)
	return &rlm
}

// getOrInsert returns an existing rate limiter from the map, or inserts a new
// one with the desired settings and returns it.
func (rlm *rateLimitersMap) getOrInsert(key string, rl *RateLimit) RateLimiter {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	rateLimiter, ok := rlm.limiters[key]
	if ok {
		// Verify the existing limiter matches the current config type
		switch l := rateLimiter.(type) {
		case *concurrencyLimiter:
			if rl.MaxConcurrent > 0 {
				// Prevent sweep() from deleting this while we are between getOrInsert and Acquire
				l.lastAccess.Store(now().UnixNano())
				return rateLimiter
			}
		case *ringBufferRateLimiter:
			if rl.MaxConcurrent == 0 {
				return rateLimiter
			}
		}
		// Type mismatch (likely due to config reload). We fall through to recreate it.
	}

	if rl.MaxConcurrent > 0 {
		rateLimiter = newConcurrencyLimiter(int64(rl.MaxConcurrent))
	} else {
		rateLimiter = newRingBufferRateLimiter(rl.MaxEvents, time.Duration(rl.Window))
	}
	rlm.limiters[key] = rateLimiter
	return rateLimiter
}

// updateAll updates existing rate limiters with new settings.
func (rlm *rateLimitersMap) updateAll(rl *RateLimit) {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	for key, limiter := range rlm.limiters {
		switch l := limiter.(type) {
		case *concurrencyLimiter:
			if rl.MaxConcurrent > 0 {
				l.limit.Store(int64(rl.MaxConcurrent))
			} else {
				delete(rlm.limiters, key)
			}
		case *ringBufferRateLimiter:
			if rl.MaxConcurrent == 0 {
				l.SetMaxEvents(rl.MaxEvents)
				l.SetWindow(time.Duration(rl.Window))
			} else {
				delete(rlm.limiters, key)
			}
		}
	}
}

// sweep cleans up expired rate limit states.
func (rlm *rateLimitersMap) sweep() {
	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()

	for key, limiter := range rlm.limiters {
		switch l := limiter.(type) {
		case *ringBufferRateLimiter:
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
			}(l)
		case *concurrencyLimiter:
			// for concurrency limiters, we can remove them if they are idle
			// and have no active requests.
			if l.current.Load() == 0 {
				lastAccess := time.Unix(0, l.lastAccess.Load())
				if now().Sub(lastAccess) > idleLimiterTTL {
					delete(rlm.limiters, key)
				}
			}
		}
	}
}

// rlStateForZone returns the state of all rate limiters in the map.
func (rlm *rateLimitersMap) rlStateForZone(timestamp time.Time) map[string]rlStateValue {
	state := make(map[string]rlStateValue)

	rlm.limitersMu.Lock()
	defer rlm.limitersMu.Unlock()
	for key, limiter := range rlm.limiters {
		switch l := limiter.(type) {
		case *ringBufferRateLimiter:
			count, oldestEvent := l.Count(timestamp)
			state[key] = rlStateValue{
				Count:       count,
				OldestEvent: oldestEvent,
			}
		case *concurrencyLimiter:
			state[key] = rlStateValue{
				Count: int(l.current.Load()),
			}
		}
	}

	return state
}
