Caddy HTTP Rate Limit Module
============================

This module implements both internal and distributed HTTP rate limiting. Requests can be rejected after a specified rate limit is hit.

**WORK IN PROGRESS:** Please note that this module is still unfinished and may have bugs. Please try it out and file bug reports - thanks!


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
- Distributed rate limiting across a cluster
- Caddyfile support


**PLANNED:**

- Ability to define matchers in zones with Caddyfile
- Smoothed estimates of distributed rate limiting
- RL state persisted in storage for resuming after restarts
- Admin API endpoints to inspect or modify rate limits


## Building

To build Caddy with this module, use [xcaddy](https://github.com/caddyserver/xcaddy):

```bash
$ xcaddy build --with github.com/mholt/caddy-ratelimit
```


## Overview

The `rate_limit` HTTP handler module lets you define rate limit zones, which have a unique name of your choosing. A rate limit zone is 1:1 with a rate limit (i.e. events per duration).

A zone also has a key, which is different from its name. Keys associate 1:1 with rate limiters, implemented as ring buffers; i.e. a new key implies allocating a new ring buffer. Keys can be static (no placeholders; same for every request), in which case only one rate limiter will be allocated for the whole zone. Or, keys can contain placeholders which can be different for every request, in which case a zone may contain numerous rate limiters depending on the result of expanding the key.

A zone is synomymous with a rate limit, being a number of events per duration. Both `window` and `max_events` are required configuration for a zone. For example: 100 events every 1 minute. Because this module uses a sliding window algorithm, it works by looking back `<window>` duration and seeing if `<max_events>` events have already happened in that timeframe. If so, an internal HTTP 429 error is generated and returned, invoking error routes which you have defined (if any). Otherwise, the a reservation is made and the event is allowed through.

Each zone may optionally filter the requests it applies to by specifying [request matchers](https://caddyserver.com/docs/modules/http#servers/routes/match).

Unlike nginx's rate limit module, this one does not require you to set a memory bound. Instead, rate limiters are scanned every so often and expired ones are deleted so their memory can be recovered by the garbage collector: Caddy does not drop rate limiters on the floor and forget events like nginx does.

### Distributed rate limiting

With a little bit more CPU, I/O, and a teensy bit more memory overhead, this module distributes its rate limit state across a cluster. A cluster is simply defined as other rate limit modules that are configured to use the same storage.

Distributed RL works by periodically writing its internal RL state to storage, while also periodically reading other instances' RL state from storage, then accounting for their states when making allowance decisions. In order for this to work, all instances in the cluster must have the exact same RL zone configurations.

This synchronization algorithm is inherently approximate, but also eventually consistent (and is similar to what other enterprise-only rate limiters do). Its performance depends heavily on parameter tuning (e.g. how often to read and write), configured rate limit windows and event maximums, and performance characteristics of the underlying storage implementation. (It will be fairly heavy on reads, but writes will be lighter, even if more frequent.)


## Syntax

This is an HTTP handler module, so it can be used wherever `http.handlers` modules are accepted.

### JSON config

```json
{
	"handler": "rate_limit",
	"rate_limits": {
		"<name>": {
			"match": [],
			"key": "",
			"window": "",
			"max_events": 0
		},
		"distributed": {
			"write_interval": "",
			"read_interval": ""
		},
		"storage": {},
		"jitter": 0.0,
		"sweep_interval": ""
	}
}
```


All fields are optional, but to be useful, you'll need to define at least one zone, and a zone requires `window` and `max_events` to be set. Keys can be static (no placeholders) or dynamic (with placeholders). Matchers can be used to filter requests that apply to a zone. Replace `<name>` with your RL zone's name.

To enable distributed RL, set `distributed` to a non-null object. The default read and write intervals are 5s, but you should tune these for your individual deployments.

Storage customizes the storage module that is used. Like normal Caddy convention, all instances with the same storage configuration are considered to be part of a cluster.

Jitter is an optional percentage that adds random variance to the Retry-After time to avoid stampeding herds.

Sweep interval configures how often to scan for expired rate limiters. The default is 1m.


### Caddyfile config

As with all non-standard HTTP handler modules, this directive is not known to the Caddyfile adapter and so it must be ["ordered"](https://caddyserver.com/docs/caddyfile/directives#directive-order) manually using global options unless it only appears within a [`route` block](https://caddyserver.com/docs/caddyfile/directives/route). This ordering usually works well, but you should use discretion:

```
{
	order rate_limit before basicauth
}
```

Here is the syntax. See the JSON config section above for explanations about each property:

```
rate_limit {
	zone <name> {
		key    <string>
		window <duration>
		events <max_events>
	}
	distributed {
		read_interval  <duration>
		write_interval <duration>
	}
	storage <module...>
	jitter  <percent>
	sweep_interval <duration>
}
```

Like with the JSON config, all subdirectives are optional and have sensible defaults (but you will obviously want to specify at least one zone).

Multiple zones can be defined. Distributed RL can be enabled just by specifying `distributed` if you want to use its default settings.

## Examples

We'll show an equivalent JSON and Caddyfile example that defines two rate limit zones: `static_example` and `dynamic_example`.

In the `static_example` zone, there is precisely one ring buffer allocated because the key is static (no placeholders), and we also demonstrate defining a matcher set to select which requests the rate limit applies to. Only 100 GET requests will be allowed through every minute, across all clients.

In the `dynamic_example` zone, the key is dynamic (has a placeholder), and in this case we're using the client's IP address (`{http.request.remote.host}`). We allow only 2 requests per client IP in the last 5 seconds from any given time.

We also enable distributed rate limiting. By deploying this config to two or more instances sharing the same storage module (which we did not define here, so Caddy's global storage config will be used), they will act approximately as one instance when making rate limiting decisions.


### JSON example

```json
{
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":80"],
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
											"window": "1m",
											"max_events": 100
										},
										"dynamic_example": {
											"key": "{http.request.remote.host}",
											"window": "5s",
											"max_events": 2
										}
									},
									"distributed": {}
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

### Caddyfile example

(The Caddyfile does not yet support defining matchers for RL zones, so that has been omitted from this example.)

```
{
	order rate_limit before basicauth
}

:80

rate_limit {
	distributed
	zone static_example {
		key    static
		events 100
		window 1m
	}
	zone dynamic_example {
		key    {remote_host}
		events 2
		window 5s
	}
}

respond "I'm behind the rate limiter!"
```
