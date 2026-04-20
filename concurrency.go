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
	"sync/atomic"
)

type concurrencyLimiter struct {
	current    atomic.Int64
	limit      atomic.Int64
	lastAccess atomic.Int64
}

func (*concurrencyLimiter) isRateLimiter() {}

func newConcurrencyLimiter(limit int64) *concurrencyLimiter {
	cl := &concurrencyLimiter{}
	cl.limit.Store(limit)
	cl.lastAccess.Store(now().UnixNano())
	return cl
}

func (cl *concurrencyLimiter) Acquire() bool {
	for {
		cur := cl.current.Load()
		if cur >= cl.limit.Load() {
			return false
		}
		if cl.current.CompareAndSwap(cur, cur+1) {
			cl.lastAccess.Store(now().UnixNano())
			return true
		}
	}
}

func (cl *concurrencyLimiter) Release() {
	cl.current.Add(-1)
	cl.lastAccess.Store(now().UnixNano())
}
