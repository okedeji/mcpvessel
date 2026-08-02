# The demo weather server

An ordinary, honest MCP server. It reports the weather using
[Open-Meteo](https://open-meteo.com), a public API that needs no key and no
account, and it does nothing you would not want it to do.

That is the point. The [notes server](../notes-server/) next door is rigged: it
reaches a host that does not exist, so the answer is always no and the cage
always looks right. This one is the case that decides whether deny-default is
usable, a server you actually want, reaching hosts you would actually approve.

Three tools:

- `get_current_weather(place, fahrenheit=False)`: conditions right now.
- `get_forecast(place, days=3, fahrenheit=False)`: a daily forecast, up to 16 days.
- `find_place(place, limit=5)`: coordinates for a place name, to tell two
  Springfields apart.

## Why it reaches two hosts

A place name has to become coordinates before a forecast can be requested, so a
call touches:

- `geocoding-api.open-meteo.com`, to resolve the name
- `api.open-meteo.com`, to fetch the forecast

Both are needed for a single `get_forecast` call, and the Vesselfile presets
neither. That is deliberate. Real servers reach more than one host, and
approving the first only to be stopped by a second is the friction a user meets
in practice. A demo that reaches exactly one host would flatter the design.

## Try it

```sh
# Build the caged server.
mcpvessel build ./demo/weather-server -t @me/weather:0.1

# Merge it into the front door mcpvessel is already serving.
mcpvessel serve add --listen 127.0.0.1:<port> --egress-inspect @me/weather:0.1
```

Then ask your agent for the weather somewhere. The server has no allowed hosts,
so the first call is stopped and the host is held for you to approve. With
`--egress-inspect`, `mcpvessel egress preview <run> <host>` shows the exact
request it was about to send, before you decide.

```sh
mcpvessel egress ls
mcpvessel egress preview <run> geocoding-api.open-meteo.com
mcpvessel egress allow @me/weather:0.1 geocoding-api.open-meteo.com
```

Approve both hosts and the tool works normally from then on, with no further
prompting: an approval is remembered for that tag.

## What this is for

It exercises the path the [notes server](../notes-server/) cannot:

- A held host that **deserves** approval, so the decision is a real one.
- The preview of a request you are about to allow, on a request that is genuinely
  benign, where the honest answer is yes.
- A second host held after the first is approved.
- An approval being remembered, so the friction is paid once rather than on
  every call.
- A tool that reports a blocked host clearly enough for an agent to explain what
  happened, instead of just failing.

The server is written to be read. It handles its own errors, names the host it
could not reach, and validates its inputs, the way any server you would actually
install should.
