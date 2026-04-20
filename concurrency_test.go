// Copyright 2026 Matthew Holt

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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

var blockChan = make(chan struct{})
var enteredChan = make(chan struct{}, 10)

func init() {
	caddy.RegisterModule(BlockHandler{})
}

type BlockHandler struct{}

func (BlockHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.test_block",
		New: func() caddy.Module { return new(BlockHandler) },
	}
}

func (h BlockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	enteredChan <- struct{}{}
	<-blockChan
	w.WriteHeader(200)
	return nil
}

func TestConcurrencyLimit(t *testing.T) {
	// Clear any leftover state
	for len(enteredChan) > 0 {
		<-enteredChan
	}

	maxConcurrent := 2
	config := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"concurrency_zone": {
										"key": "static",
										"max_concurrent": %d
									}
								}
							},
							{
								"handler": "test_block"
							}
						]
					}]
				}
			}
		}
	}
}`, maxConcurrent)

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	var wg sync.WaitGroup
	reqDone := make(chan struct{}, 10)
	// Start maxConcurrent requests
	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { reqDone <- struct{}{} }()
			resp, err := http.Get("http://localhost:8080")
			if err != nil {
				t.Errorf("request failed: %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("expected status 200, got %d", resp.StatusCode)
			}
		}()
	}

	// Wait for requests to be blocked
	for i := 0; i < maxConcurrent; i++ {
		<-enteredChan
	}

	// Next request should be rate limited (429) immediately
	tester.AssertGetResponse("http://localhost:8080", 429, "")

	// Release one request
	blockChan <- struct{}{}

	// Wait for the released request to finish and for the limiter to be updated
	<-reqDone

	// Now we should be able to make one more request that blocks
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { reqDone <- struct{}{} }()
		resp, err := http.Get("http://localhost:8080")
		if err != nil {
			t.Errorf("request failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	}()

	// Wait until it blocks
	<-enteredChan

	// Still at limit
	tester.AssertGetResponse("http://localhost:8080", 429, "")

	// Release all (2 more)
	blockChan <- struct{}{}
	blockChan <- struct{}{}
	wg.Wait()

	// Now it should be free
	go func() { blockChan <- struct{}{} }()
	resp, err := http.Get("http://localhost:8080")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	<-enteredChan // Consume the leftover signal
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestConcurrencyLimitMultipleKeys(t *testing.T) {
	// Clear any leftover state
	for len(enteredChan) > 0 {
		<-enteredChan
	}

	maxConcurrent := 1
	config := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"concurrency_zone": {
										"key": "{http.request.orig_uri.path}",
										"max_concurrent": %d
									}
								}
							},
							{
								"handler": "test_block"
							}
						]
					}]
				}
			}
		}
	}
}`, maxConcurrent)

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	var wg sync.WaitGroup
	// Start request for path1
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get("http://localhost:8080/path1")
		if err != nil {
			t.Errorf("request failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	}()

	// Wait for request to be blocked
	<-enteredChan

	// path1 should be limited
	tester.AssertGetResponse("http://localhost:8080/path1", 429, "")

	// path2 should NOT be limited
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get("http://localhost:8080/path2")
		if err != nil {
			t.Errorf("request failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	}()

	// Wait for request to be blocked
	<-enteredChan

	// path2 should now be limited
	tester.AssertGetResponse("http://localhost:8080/path2", 429, "")

	// Release all
	blockChan <- struct{}{}
	blockChan <- struct{}{}
	wg.Wait()
}

func TestMixedZones(t *testing.T) {
	// Clear any leftover state
	for len(enteredChan) > 0 {
		<-enteredChan
	}

	config := `{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"mixed": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"window_zone": {
										"key": "static",
										"window": "10s",
										"max_events": 1
									},
									"concurrency_zone": {
										"key": "static",
										"max_concurrent": 1
									}
								}
							},
							{
								"handler": "test_block"
							}
						]
					}]
				}
			}
		}
	}
}`

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	// First request should pass both and block
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get("http://localhost:8080")
		if err != nil {
			t.Errorf("request failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("expected status 200, got %d", resp.StatusCode)
		}
	}()

	// Wait for request to be blocked
	<-enteredChan

	// Second request should be rate limited
	tester.AssertGetResponse("http://localhost:8080", 429, "")

	// Release first request
	blockChan <- struct{}{}
	wg.Wait()

	// Now concurrency is free, but window zone is still limited (10s window)
	tester.AssertGetResponse("http://localhost:8080", 429, "")
}

func TestConcurrencyRollback(t *testing.T) {
	// Clear any leftover state
	for len(enteredChan) > 0 {
		<-enteredChan
	}

	config := `{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"rollback": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"concurrency_zone": {
										"key": "static",
										"max_concurrent": 1
									},
									"fail_zone": {
										"key": "static",
										"window": "1m",
										"max_events": 1
									}
								}
							},
							{
								"handler": "static_response",
								"body": "success"
							}
						]
					}]
				}
			}
		}
	}
}`

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	// 1st request consumes the single event in fail_zone.
	// Since static_response doesn't block, concurrency is immediately released.
	tester.AssertGetResponse("http://localhost:8080", 200, "success")

	// 2nd request passes concurrency_zone (count becomes 1)
	// but fails fail_zone (max_events 1 is already consumed).
	// It returns 429 and the concurrency_zone count should rollback to 0.
	resp2, err := http.Get("http://localhost:8080")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 429 {
		t.Errorf("expected 429, got %d", resp2.StatusCode)
	}
	// A request blocked by a window_zone will have a Retry-After header.
	if resp2.Header.Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header from fail_zone")
	}

	// 3rd request: If rollback succeeded, it will again pass concurrency_zone
	// and fail at fail_zone, returning a 429 WITH a Retry-After header.
	// If rollback failed, it will be rejected by concurrency_zone, returning
	// a 429 WITHOUT a Retry-After header (since wait time is 0 for concurrency).
	resp3, err := http.Get("http://localhost:8080")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 429 {
		t.Errorf("expected 429, got %d", resp3.StatusCode)
	}
	if resp3.Header.Get("Retry-After") == "" {
		t.Errorf("concurrency rollback failed: request was rejected by concurrency_zone (missing Retry-After header)")
	}
}

func TestConfigReloadPanic(t *testing.T) {
	// First provision a window zone
	rl1 := &RateLimit{
		Window:    caddy.Duration(time.Second),
		MaxEvents: 10,
	}
	rl1.limitersMap = newRateLimiterMap()
	rl1.provision(caddy.Context{}, "panic_zone")

	// Simulate a request to populate the map
	rl1.limitersMap.getOrInsert("mykey", rl1)

	// Now provision the same zone name as a concurrency zone (simulating config reload)
	rl2 := &RateLimit{
		MaxConcurrent: 5,
	}
	rl2.limitersMap = newRateLimiterMap()
	rl2.provision(caddy.Context{}, "panic_zone")

	// This should not panic if correctly handled
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Panicked! %v", r)
		}
	}()

	// Simulate a request with the new config
	limiter := rl2.limitersMap.getOrInsert("mykey", rl2)
	_ = limiter.(*concurrencyLimiter) // This will panic because it's a ringBufferRateLimiter
}
