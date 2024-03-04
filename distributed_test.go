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
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/caddyserver/certmagic"
	"github.com/google/uuid"
)

func TestDistributed(t *testing.T) {
	initTime()
	window := 60
	maxEvents := 10

	// Make sure AppDataDir exists, because otherwise the caddytest.Tester won't
	// be able to generate an instance ID
	if err := os.MkdirAll(caddy.AppDataDir(), 0700); err != nil {
		t.Fatalf("failed to create app data dir %s: %s", caddy.AppDataDir(), err)
	}

	testCases := []struct {
		name               string
		peerRequests       int
		peerStateTimeStamp time.Time
		localRequests      int
		rateLimited        bool
	}{
		// Request should be refused because a peer used up the rate limit
		{
			name:               "peer-usage-in-window",
			peerRequests:       maxEvents,
			peerStateTimeStamp: now(),
			localRequests:      0,
			rateLimited:        true,
		},
		// Request should be allowed because while lots of requests are in the
		// peer state, the timestamp is outside the window
		{
			name:               "peer-usage-before-window",
			peerStateTimeStamp: now().Add(-time.Duration(window + 1)),
			localRequests:      0,
			rateLimited:        false,
		},
		// Request should be refused because local usage exceeds rate limit
		{
			name:               "local-usage",
			peerRequests:       0,
			peerStateTimeStamp: now(),
			localRequests:      maxEvents,
			rateLimited:        true,
		},
		// Request should be refused because usage in peer and locally sum up to
		// exceed rate limit
		{
			name:               "both-usage",
			peerRequests:       maxEvents / 2,
			peerStateTimeStamp: now(),
			localRequests:      maxEvents / 2,
			rateLimited:        true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			storageDir := t.TempDir()
			// Use a random UUID as the zone so that rate limits from multiple test runs
			// collide with each other
			zone := uuid.New().String()

			// To simulate a peer in a rate limiting cluster, constuct a
			// ringBufferRateLimiter, record a bunch of events in it, and then sync that
			// state to storage.
			parsedDuration, err := time.ParseDuration(fmt.Sprintf("%ds", window))
			if err != nil {
				t.Fatal("failed to parse duration")
			}
			simulatedPeer := newRingBufferRateLimiter(maxEvents, parsedDuration)

			for i := 0; i < testCase.peerRequests; i++ {
				if when := simulatedPeer.When(); when != 0 {
					t.Fatalf("event should be allowed")
				}
			}

			zoneLimiters := newRateLimiterMap()
			zoneLimiters.limiters["static"] = simulatedPeer

			rlState := rlState{
				Timestamp: testCase.peerStateTimeStamp,
				Zones: map[string]map[string]rlStateValue{
					zone: zoneLimiters.rlStateForZone(now()),
				},
			}

			storage := certmagic.FileStorage{
				Path: storageDir,
			}

			if err := writeRateLimitState(context.Background(), rlState, "f92a00f1-050c-4353-83b1-8ccc2337c25b", &storage); err != nil {
				t.Fatalf("failed to write state to storage: %s", err)
			}

			// For Windows, escape \ in storage path.
			storageDir = strings.ReplaceAll(storageDir, `\`, `\\`)

			// Run a caddytest.Tester that uses the same storage we just wrote to, so it
			// will treat the generated state as a peer to sync from.
			configString := `{
	"admin": {"listen": "localhost:2999"},
	"storage": {
		"module": "file_system",
		"root": "%s"
	},
	"logging": {
		"logs": {
			"default": {
				"level": "DEBUG"
			}
		}
	},
	"apps": {
		"http": {
			"servers": {
				"one": {
					"listen": [":8080"],
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"%s": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "%ds",
										"max_events": %d
									}
								},
								"distributed": {
									"write_interval": "3600s",
									"read_interval": "3600s"
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
}`

			testerConfig := fmt.Sprintf(configString, storageDir, zone, window, maxEvents)
			tester := caddytest.NewTester(t)
			tester.InitServer(testerConfig, "json")

			for i := 0; i < testCase.localRequests; i++ {
				tester.AssertGetResponse("http://localhost:8080", 200, "")
			}

			if testCase.rateLimited {
				assert429Response(t, tester, int64(window))
			} else {
				tester.AssertGetResponse("http://localhost:8080", 200, "")
			}
		})
	}
}
