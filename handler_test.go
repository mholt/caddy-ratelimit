// Copyright 2023 Matthew Holt

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//  http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddyrl

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
)

const referenceTime = 1000000

func initTime() {
	now = func() time.Time {
		return time.Unix(referenceTime, 0)
	}
}

func advanceTime(seconds int) {
	now = func() time.Time {
		return time.Unix(referenceTime+int64(seconds), 0)
	}
}

func assert429Response(t *testing.T, tester *caddytest.Tester, expectedRetryAfter int64) {
	response, _ := tester.AssertGetResponse("http://localhost:8080", 429, "")

	retry_after := response.Header.Get("retry-after")
	if retry_after == "" {
		t.Fatal("429 response should have retry-after header")
	}

	retry_after_int, err := strconv.ParseInt(retry_after, 10, 64)
	if err != nil {
		t.Fatalf("could not parse retry-after header as integer: %+v", retry_after)
	}

	if retry_after_int != expectedRetryAfter {
		t.Fatalf("unexpected retry-after header value: %+v (wanted %d)", retry_after, expectedRetryAfter)
	}
}

func TestRateLimits(t *testing.T) {
	window := 60
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
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
									"zone1": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "%ds",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, window, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	assert429Response(t, tester, int64(window))

	// After advancing time by half the window, the retry-after value should
	// change accordingly
	advanceTime(window / 2)

	assert429Response(t, tester, int64(window/2))

	// Advance time beyond the window where the events occurred. We should now
	// be able to make requests again.
	advanceTime(window)

	tester.AssertGetResponse("http://localhost:8080", 200, "")
}

func TestDistinctZonesAndKeys(t *testing.T) {
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
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
									"zone1": {
										"match": [{"method": ["GET"]}],
										"key": "{http.request.orig_uri.path}",
										"window": "60s",
										"max_events": %d
									},
									"zone2": {
										"match": [{"method": ["DELETE"]}],
										"key": "{http.request.orig_uri.path}",
										"window": "60s",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, maxEvents, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	// Rate limits for different zones (by method) and keys (by request path)
	// should be accounted independently
	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080/path1", 200, "")
	}
	tester.AssertGetResponse("http://localhost:8080/path1", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080/path2", 200, "")
	}
	tester.AssertGetResponse("http://localhost:8080/path2", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertDeleteResponse("http://localhost:8080/path1", 200, "")
	}
	tester.AssertDeleteResponse("http://localhost:8080/path1", 429, "")

	for i := 0; i < maxEvents; i++ {
		tester.AssertDeleteResponse("http://localhost:8080/path2", 200, "")
	}
	tester.AssertDeleteResponse("http://localhost:8080/path2", 429, "")
}
