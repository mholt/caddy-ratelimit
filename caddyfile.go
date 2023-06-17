// Copyright 2021 Matthew Holt

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
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("rate_limit", parseCaddyfile)
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(helper httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var h Handler
	err := h.UnmarshalCaddyfile(helper.Dispenser)
	return h, err
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler. Syntax:
//
//	rate_limit {
//	    zone <name> {
//	        key    <string>
//	        window <duration>
//	        events <max_events>
//	    }
//	    distributed {
//	        read_interval  <duration>
//	        write_interval <duration>
//	    }
//	    storage <module...>
//	    jitter  <percent>
//	    sweep_interval <duration>
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}

		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "zone":
				if !d.NextArg() {
					return d.ArgErr()
				}
				zoneName := d.Val()

				var zone RateLimit
				for nesting := d.Nesting(); d.NextBlock(nesting); {
					switch d.Val() {
					case "key":
						if !d.NextArg() {
							return d.ArgErr()
						}
						if zone.Key != "" {
							return d.Errf("zone key already specified: %s", zone.Key)
						}
						zone.Key = d.Val()

					case "window":
						if !d.NextArg() {
							return d.ArgErr()
						}
						if zone.Window != 0 {
							return d.Errf("zone window already specified: %v", zone.Window)
						}
						window, err := caddy.ParseDuration(d.Val())
						if err != nil {
							return d.Errf("invalid window duration '%s': %v", d.Val(), err)
						}
						zone.Window = caddy.Duration(window)

					case "events":
						if !d.NextArg() {
							return d.ArgErr()
						}
						if zone.MaxEvents != 0 {
							return d.Errf("zone max events already specified: %v", zone.MaxEvents)
						}
						maxEvents, err := strconv.Atoi(d.Val())
						if err != nil {
							return d.Errf("invalid max events integer '%s': %v", d.Val(), err)
						}
						zone.MaxEvents = maxEvents
					}
				}
				if zone.Window == 0 || zone.MaxEvents == 0 {
					return d.Err("a rate limit zone requires both a window and maximum events")
				}

				if h.RateLimits == nil {
					h.RateLimits = make(map[string]*RateLimit)
				}
				h.RateLimits[zoneName] = &zone

			case "distributed":
				h.Distributed = new(DistributedRateLimiting)

				for nesting := d.Nesting(); d.NextBlock(nesting); {
					switch d.Val() {
					case "read_interval":
						if !d.NextArg() {
							return d.ArgErr()
						}
						if h.Distributed.ReadInterval != 0 {
							return d.Errf("read interval already specified: %v", h.Distributed.ReadInterval)
						}
						interval, err := caddy.ParseDuration(d.Val())
						if err != nil {
							return d.Errf("invalid read interval '%s': %v", d.Val(), err)
						}
						h.Distributed.ReadInterval = caddy.Duration(interval)

					case "write_interval":
						if !d.NextArg() {
							return d.ArgErr()
						}
						if h.Distributed.WriteInterval != 0 {
							return d.Errf("write interval already specified: %v", h.Distributed.WriteInterval)
						}
						interval, err := caddy.ParseDuration(d.Val())
						if err != nil {
							return d.Errf("invalid write interval '%s': %v", d.Val(), err)
						}
						h.Distributed.WriteInterval = caddy.Duration(interval)
					}
				}

			case "storage":
				if !d.NextArg() {
					return d.ArgErr()
				}
				modID := "caddy.storage." + d.Val()
				unm, err := caddyfile.UnmarshalModule(d, modID)
				if err != nil {
					return err
				}
				storage, ok := unm.(caddy.StorageConverter)
				if !ok {
					return d.Errf("module %s is not a caddy.StorageConverter", modID)
				}
				h.StorageRaw = caddyconfig.JSONModuleObject(storage, "module", storage.(caddy.Module).CaddyModule().ID.Name(), nil)

			case "jitter":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if h.Jitter != 0 {
					return d.Errf("jitter already specified: %v", h.Jitter)
				}
				jitter, err := strconv.ParseFloat(d.Val(), 64)
				if err != nil {
					return d.Errf("invalid jitter percentage '%s': %v", d.Val(), err)
				}
				h.Jitter = jitter

			case "sweep_interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if h.SweepInterval != 0 {
					return d.Errf("sweep interval already specified: %v", h.SweepInterval)
				}
				interval, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return d.Errf("invalid sweep interval '%s': %v", d.Val(), err)
				}
				h.SweepInterval = caddy.Duration(interval)
			}
		}
	}

	return nil
}
