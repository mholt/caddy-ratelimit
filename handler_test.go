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

func TestApplyNetworkPrefix(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		ipv4Prefix int
		ipv6Prefix int
		expected   string
	}{
		// IPv6 with /64 prefix - different addresses in the same /64 produce the same key
		{"ipv6 /64 full addr", "2001:db8:1234:5678:9abc:def0:1234:5678", 0, 64, "2001:db8:1234:5678::/64"},
		{"ipv6 /64 all ones host", "2001:db8:1234:5678:ffff:ffff:ffff:ffff", 0, 64, "2001:db8:1234:5678::/64"},
		{"ipv6 /64 short addr", "2001:db8:1234:5678::1", 0, 64, "2001:db8:1234:5678::/64"},

		// IPv6 with other prefix lengths
		{"ipv6 /48", "2001:db8:1234:5678::1", 0, 48, "2001:db8:1234::/48"},
		{"ipv6 /128", "2001:db8:1234:5678::1", 0, 128, "2001:db8:1234:5678::1/128"},

		// IPv4 with prefix configured
		{"ipv4 /24", "192.168.1.100", 24, 0, "192.168.1.0/24"},
		{"ipv4 /8", "10.0.0.50", 8, 0, "10.0.0.0/8"},
		{"ipv4 /32", "172.16.5.4", 32, 0, "172.16.5.4/32"},

		// Both prefixes configured - each applies to its own address family
		{"ipv6 with both configured", "2001:db8::1", 24, 64, "2001:db8::/64"},
		{"ipv4 with both configured", "192.168.1.100", 24, 64, "192.168.1.0/24"},

		// Only IPv6 prefix configured - IPv4 addresses pass through unchanged
		{"ipv4 unchanged when only ipv6 configured", "192.168.1.100", 0, 64, "192.168.1.100"},

		// Only IPv4 prefix configured - IPv6 addresses pass through unchanged
		{"ipv6 unchanged when only ipv4 configured", "2001:db8::1", 24, 0, "2001:db8::1"},

		// No prefixes configured - everything passes through unchanged
		{"ipv6 no prefix", "2001:db8::1", 0, 0, "2001:db8::1"},
		{"ipv4 no prefix", "192.168.1.100", 0, 0, "192.168.1.100"},

		// Non-IP keys always pass through unchanged
		{"static key", "static", 0, 64, "static"},
		{"placeholder key", "{http.request.uri}", 0, 64, "{http.request.uri}"},
		{"empty key", "", 0, 64, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := applyNetworkPrefix(tt.key, tt.ipv4Prefix, tt.ipv6Prefix)
			if result != tt.expected {
				t.Errorf("applyNetworkPrefix(%q, %d, %d) = %q, want %q",
					tt.key, tt.ipv4Prefix, tt.ipv6Prefix, result, tt.expected)
			}
		})
	}
}

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
