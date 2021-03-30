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

// Current time function, to be substituted by tests
var now = time.Now
