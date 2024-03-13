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
	"testing"
	"time"
)

func TestCount(t *testing.T) {
	initTime()

	var zeroTime time.Time
	bufSize := 10
	rb := newRingBufferRateLimiter(bufSize, time.Duration(bufSize)*time.Second)
	startTime := now()

	count, oldest := rb.Count(now())
	if count != 0 {
		t.Fatalf("count should be 0 for empty ring buffer")
	}
	if oldest != zeroTime {
		t.Fatalf("oldest event should be zero value for empty ring buffer")
	}

	// Fill the buffer with events spaced out by 1s
	for i := 0; i < bufSize; i++ {
		advanceTime(i)
		if when := rb.When(); when != 0 {
			t.Fatalf("empty ring buffer should allow events")
		}

		count, oldest = rb.Count(now())
		if count != i+1 {
			t.Fatalf("count %d is wrong", count)
		}
		if oldest != startTime {
			t.Fatalf("oldest time %+v is wrong", oldest)
		}
	}

	if when := rb.When(); when != time.Second {
		t.Fatal("full ring buffer should forbid events")
	}

	count, oldest = rb.Count(now())
	if count != bufSize {
		t.Fatalf("count %d is wrong", count)
	}
	if oldest != startTime {
		t.Fatalf("oldest time %+v is wrong", oldest)
	}

	// Advance time by half the window. Only half the events should be counted,
	// and the oldest event should be updated.
	advanceTime(bufSize + bufSize/2)

	count, oldest = rb.Count(now())
	if count != bufSize/2 {
		t.Fatalf("count %d is wrong", count)
	}
	if oldest != startTime.Add(time.Duration(bufSize/2)*time.Second) {
		t.Fatalf("oldest time %+v is wrong", oldest)
	}

	// Advance by the whole window. There should now be no events in the window
	advanceTime(2 * bufSize)

	count, oldest = rb.Count(now())
	if count != 0 {
		t.Fatalf("count %d is wrong", count)
	}
	if oldest != zeroTime {
		t.Fatalf("oldest time %+v is wrong", oldest)
	}
}
