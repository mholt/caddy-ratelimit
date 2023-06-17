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
	"sync"
	"time"
)

// ringBufferRateLimiter uses a ring to enforce rate limits
// consisting of a maximum number of events within a single
// sliding window of a given duration. An empty value is
// not valid; always call initialize() before using.
type ringBufferRateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	ring   []time.Time // len(ring) == maxEvents
	cursor int         // always points to the oldest timestamp
}

// initialize sets up the rate limiter if it isn't already, allowing maxEvents
// in a sliding window of size window. If maxEvents is 0, no events are
// allowed. If window is 0, all events are allowed. It panics if maxEvents or
// window are less than zero. This method is idempotent.
func (r *ringBufferRateLimiter) initialize(maxEvents int, window time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.window != 0 || r.ring != nil {
		return
	}
	if maxEvents < 0 {
		panic("maxEvents cannot be less than zero")
	}
	if window < 0 {
		panic("window cannot be less than zero")
	}
	r.window = window
	r.ring = make([]time.Time, maxEvents) // TODO: we can probably pool these
}

// When returns the duration before the next allowable event; it does not block.
// If zero, the event is allowed and a reservation is immediately made.
// If non-zero, the event is NOT allowed and a reservation is not made.
func (r *ringBufferRateLimiter) When() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.allowed() {
		return 0
	}
	return time.Until(r.ring[r.cursor].Add(r.window))
}

// allowed returns true if the event is allowed to happen right now.
// It does not wait. If the event is allowed, a reservation is made.
// It is NOT safe for concurrent use, so it must be called inside a
// lock on r.mu.
func (r *ringBufferRateLimiter) allowed() bool {
	if len(r.ring) == 0 {
		return false
	}
	if time.Since(r.ring[r.cursor]) > r.window {
		r.reserve()
		return true
	}
	return false
}

// reserve claims the current spot in the ring buffer
// and advances the cursor.
// It is NOT safe for concurrent use, so it must
// be called inside a lock on r.mu.
func (r *ringBufferRateLimiter) reserve() {
	r.ring[r.cursor] = now()
	r.advance()
}

// advance moves the cursor to the next position.
// It is NOT safe for concurrent use, so it must
// be called inside a lock on r.mu.
func (r *ringBufferRateLimiter) advance() {
	r.cursor++
	if r.cursor >= len(r.ring) {
		r.cursor = 0
	}
}

// MaxEvents returns the maximum number of events that
// are allowed within the sliding window.
func (r *ringBufferRateLimiter) MaxEvents() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ring)
}

// SetMaxEvents changes the maximum number of events that are
// allowed in the sliding window. If the new limit is lower,
// the oldest events will be forgotten. If the new limit is
// higher, the window will suddenly have capacity for new
// reservations. It panics if maxEvents is less than 0.
func (r *ringBufferRateLimiter) SetMaxEvents(maxEvents int) {
	if maxEvents < 0 {
		panic("maxEvents cannot be less than zero")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// only make a change if the new limit is different
	if maxEvents == len(r.ring) {
		return
	}

	newRing := make([]time.Time, maxEvents)

	// the new ring may be smaller; fast-forward to the
	// oldest timestamp that will be kept in the new
	// ring so the oldest ones are forgotten and the
	// newest ones will be remembered
	sizeDiff := len(r.ring) - maxEvents
	for i := 0; i < sizeDiff; i++ {
		r.advance()
	}

	if len(r.ring) > 0 {
		// copy timestamps into the new ring until we
		// have either copied all of them or have reached
		// the capacity of the new ring
		startCursor := r.cursor
		for i := 0; i < len(newRing); i++ {
			newRing[i] = r.ring[r.cursor]
			r.advance()
			if r.cursor == startCursor {
				// new ring is larger than old one;
				// "we've come full circle"
				break
			}
		}
	}

	r.ring = newRing
	r.cursor = 0
}

// Window returns the size of the sliding window.
func (r *ringBufferRateLimiter) Window() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.window
}

// SetWindow changes r's sliding window duration to window.
// It panics if window is less than zero.
func (r *ringBufferRateLimiter) SetWindow(window time.Duration) {
	if window < 0 {
		panic("window cannot be less than zero")
	}
	r.mu.Lock()
	r.window = window
	r.mu.Unlock()
}

// Count counts how many events are in the window from the reference time.
func (r *ringBufferRateLimiter) Count(ref time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.countUnsynced(ref)
}

// countUnsycned counts how many events are in the window from the reference time.
// It is NOT safe to use without a lock on r.mu.
// TODO: this is currently O(n) but could probably become O(log n) if we switch to some weird, custom binary search modulo ring length around the cursor.
func (r *ringBufferRateLimiter) countUnsynced(ref time.Time) int {
	beginningOfWindow := ref.Add(-r.window)

	// This loop is a little gnarly, I know. We start at one before the cursor because that's
	// the newest event, and we're trying to count how many events are in the window; so
	// iterating backwards from the cursor and wrapping around to the end of the ring is the
	// same as iterating events in reverse chronological order starting with most recent. When
	// we encounter the first element that's outside the window, then eventsInWindow has the
	// correct count of events within the window.
	for eventsInWindow := 0; eventsInWindow < len(r.ring); eventsInWindow++ {
		// start at cursor, add difference between ring length and offset (eventsInWindow),
		// then subtract 1 because we want to start 1 before cursor (newest event), then
		// modulus the ring length to wrap around if necessary
		i := (r.cursor + (len(r.ring) - eventsInWindow - 1)) % len(r.ring)
		if r.ring[i].Before(beginningOfWindow) {
			return eventsInWindow
		}
	}

	// if we looped the entire ring, all events are within the window
	return len(r.ring)
}

// Current time function, to be substituted by tests
var now = time.Now
