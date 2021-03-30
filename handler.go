package caddyrl

import (
	"encoding/json"
	"fmt"
	weakrand "math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements rate limiting features.
type Handler struct {
	// RateLimits contains the definitions of the rate limit groups, keyed by name.
	// The name **MUST** be globally unique across all other instances of this handler.
	RateLimits map[string]*RateLimit `json:"rate_limits,omitempty"`

	// Percentage jitter on expiration times (example: 0.2 means 20% jitter)
	Jitter float64 `json:"jitter,omitempty"`

	// Storage backend through which rate limit state is synced.
	StorageRaw json.RawMessage `json:"storage,omitempty" caddy:"namespace=caddy.storage inline_key=module"`

	// How often to scan for expired rate limit states. Default: 1m.
	SweepInterval caddy.Duration `json:"sweep_interval,omitempty"`

	storage certmagic.Storage
	random  *weakrand.Rand
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.rate_limit",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	for name, rl := range h.RateLimits {
		err := rl.provision(ctx, name)
		if err != nil {
			return fmt.Errorf("setting up rate limit %s: %v", name, err)
		}
	}

	if len(h.StorageRaw) > 0 {
		val, err := ctx.LoadModule(h, "StorageRaw")
		if err != nil {
			return fmt.Errorf("loading storage module: %v", err)
		}
		stor, err := val.(caddy.StorageConverter).CertMagicStorage()
		if err != nil {
			return fmt.Errorf("creating storage value: %v", err)
		}
		h.storage = stor
	}

	if h.Jitter < 0 {
		return fmt.Errorf("jitter must be at least zero")
	} else if h.Jitter > 0 {
		h.random = weakrand.New(weakrand.NewSource(now().UnixNano()))
	}

	// clean up old rate limiters while handler is running
	if h.SweepInterval == 0 {
		h.SweepInterval = caddy.Duration(1 * time.Minute)
	}
	go h.sweepRateLimiters(ctx)

	return nil
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	for name, rl := range h.RateLimits {
		// ignore rate limit if request doesn't qualify
		if !rl.matcherSets.AnyMatch(r) {
			continue
		}

		key := repl.ReplaceAll(rl.Key, "")

		// the API for sync.Pool is unfortunate: there is no LoadOrNew() method
		// which allocates/constructs a value only if needed, so we always need
		// to pre-allocate the value even if we never use it; we should be able
		// to relieve some memory pressure by putting unused values back into a
		// pool...
		limiter := ringBufPool.Get().(*ringBufferRateLimiter)
		if val, loaded := rl.limiters.LoadOrStore(key, limiter); loaded {
			ringBufPool.Put(limiter) // didn't use; save for next time
			limiter = val.(*ringBufferRateLimiter)
		} else {
			// as another side-effect of sync.Map's bad API, don't go to all
			// the work of initializing the ring buffer unless we have to
			limiter.initialize(rl.MaxEvents, time.Duration(rl.Window))
		}

		if dur := limiter.When(); dur > 0 {
			// hit rate limit!

			// add jitter, if configured
			if h.random != nil {
				jitter := h.randomFloatInRange(0, float64(dur)*h.Jitter)
				dur += time.Duration(jitter)
			}

			// add 0.5 to ceil() instead of round() which FormatFloat() does automatically
			w.Header().Set("Retry-After", strconv.FormatFloat(dur.Seconds()+0.5, 'f', 0, 64))

			// make some information about this rate limit available
			repl.Set("http.rate_limit.name", name)

			return caddyhttp.Error(http.StatusTooManyRequests, nil)
		}
	}
	return next.ServeHTTP(w, r)
}

// Cleanup cleans up the handler.
func (h *Handler) Cleanup() error {
	// remove unused rate limit zones
	for name := range h.RateLimits {
		rateLimits.Delete(name)
	}
	return nil
}

func (h Handler) randomFloatInRange(min, max float64) float64 {
	if h.random == nil {
		return 0
	}
	return min + h.random.Float64()*(max-min)
}

func (h Handler) sweepRateLimiters(ctx caddy.Context) {
	cleanerTicker := time.NewTicker(time.Duration(h.SweepInterval))
	defer cleanerTicker.Stop()

	for {
		select {
		case <-cleanerTicker.C:
			// iterate all rate limit zones
			rateLimits.Range(func(key, value interface{}) bool {
				rlMap := value.(*sync.Map)

				// iterate all static and dynamic rate limiters within zone
				rlMap.Range(func(key, value interface{}) bool {
					if value == nil {
						return true
					}
					rl := value.(*ringBufferRateLimiter)

					rl.mu.Lock()
					// no point in keeping a ring buffer of size 0 around
					if len(rl.ring) == 0 {
						rl.mu.Unlock()
						rlMap.Delete(key)
						return true
					}
					// get newest event in ring (should come right before oldest)
					cursorNewest := rl.cursor - 1
					if cursorNewest < 0 {
						cursorNewest = len(rl.ring) - 1
					}
					newest := rl.ring[cursorNewest]
					window := rl.window
					rl.mu.Unlock()

					// if newest event in memory is outside the window,
					// the entire ring has expired and can be forgotten
					if newest.Add(window).Before(now()) {
						rlMap.Delete(key)
					}

					return true
				})

				return true
			})

		case <-ctx.Done():
			return
		}
	}
}

var rateLimits = caddy.NewUsagePool()

var ringBufPool = sync.Pool{
	New: func() interface{} {
		return new(ringBufferRateLimiter)
	},
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
