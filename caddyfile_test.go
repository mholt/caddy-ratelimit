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
	"bytes"
	"fmt"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

func TestCaddyfileRateLimits(t *testing.T) {
	window := 60
	maxEvents := 10
	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
	config := fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}

	localhost:8080
	
	rate_limit {
		zone zone1 {
			match {
				method GET
			}
			key static
			window %ds
			events %d
		}
	}

	respond 200
	`, window, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "caddyfile")

	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	assert429Response(t, tester, int64(window))
	tester.AssertPostResponseBody("http://localhost:8080", nil, &bytes.Buffer{}, 200, "")

	// After advancing time by half the window, the retry-after value should
	// change accordingly
	advanceTime(window / 2)

	assert429Response(t, tester, int64(window/2))

	// Advance time beyond the window where the events occurred. We should now
	// be able to make requests again.
	advanceTime(window)

	tester.AssertGetResponse("http://localhost:8080", 200, "")
}
