Caddy HTTP Rate Limit Module
============================

**WORK IN PROGRESS:** This module implements HTTP rate limiting. Requests can be rejected after a specified rate limit is hit.

Please note that this module is still unfinished and may have bugs. Please try it out and file bug reports, thanks!


## Features

- Multiple rate limit zones
- Sliding window algorithm
- Scalable ring buffer implementation
	- Buffer pooling
	- Goroutines: 1 (to clean up old buffers)
	- Memory `O(Kn)` where:
		- `K` = events allowed in window (constant, configurable)
		- `n` = number of rate limits allocated in zone (configured by zone key; constant or dynamic)
- RL state persisted through config reloads
- Automatically sets Retry-After header
- Optional jitter for retry times
- Configurable memory management


**PLANNED:**

- Caddyfile support
- Distributed rate limiting
- Persist RL state in storage
- Automatic fractioning of rate limits in distributed cluster
- Admin API endpoints to inspect or modify rate limits


## Explanation

The `rate_limit` HTTP handler module lets you define rate limit zones, which have a unique name of your choosing.

A zone also has a key, which is different from its name. Keys associate 1:1 with rate limiters (implemented as ring buffers), i.e. a new key implies allocating a new rate limiter. Keys can be static (no placeholders; same for every request), in which case only one rate limiter will be allocated for the whole zone. Or, keys can contain placeholders which can be different for every request, in which case a zone may contain numerous rate limiters depending on the sampling of key it observes).

Every zone must also have a window and max_events, e.g. 100 events every 1 minute. Because this module uses a sliding window algorithm, it works by looking back `<window>` duration (1 minute) and seeing if 100 events have already happened in that timeframe. If so, an internal HTTP 429 error is generated, invoking error routes which you have defined (if any). Otherwise, the event is recorded and allowed through.

Each zone may optionally filter the requests it applies to by specifying request matchers.

Unlike nginx's rate limiter, this one does not require you to set a memory limit. Instead, expired rate limiters are scanned every so often and deleted so their memory can be recovered by the garbage collector.


## Example

The Caddy JSON config below defines two rate limit zones: `static_example` and `dynamic_example`.

In the `static_example` zone, there is precisely one ring buffer allocated because the key is static (no placeholders) and we also demonstrate defining a matcher set to select which requests the rate limit applies to. Only 2 GET requests will be allowed through every second, across all clients.

In the `dynamic_example` zone, the key is dynamic (has a placeholder), and in this case we're using the client's IP address (`{http.request.remote.host}`). We allow only 5 request per client IP in the last 10 seconds at any given time.

This example also configures optional values:
	- a jitter of 20%, which adds up to 20% of the backoff time to the client's response, to help avoid stampeding herd.
	- a custom sweep interval of 30 seconds. This scans all rate limiters and deletes expired ones, allowing memory to be freed later by the garbage collector. The default is 1 minute.


```json
{
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":1234"],
					"routes": [
						{
							"handle": [
								{
									"handler": "rate_limit",
									"rate_limits": {
										"static_example": {
											"match": [
												{"method": ["GET"]}
											],
											"key": "static",
											"window": "1s",
											"max_events": 2
										},
										"dynamic_example": {
											"key": "{http.request.remote.host}",
											"window": "10s",
											"max_events": 5
										},
										"jitter": 0.2,
										"sweep_interval": "30s"
									}
								},
								{
									"handler": "static_response",
									"body": "I'm behind the rate limiter!"
								}
							]
						}
					]
				}
			}
		}
	}
}
```
