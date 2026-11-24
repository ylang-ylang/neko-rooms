# Docs

## Networking modes

`NEKO_ROOMS_NETWORK_MODE=direct` is the default; the public per-room port/NAT guidance below describes **direct** mode. For the relay-capable fork's fixed HTTPS + TURN/TLS listeners, private TURN allocation pool, required real gateway/domain/certificate, and explicit activation/rollback, use the [relay deployment example](../examples/relay/). Switching the manager's mode does not automatically recreate/migrate manager-created dynamic rooms; its policy applies only to new/recreated rooms. TrueNAS activation/rollback uses official `app.update` with Custom App YAML and recreates all App-owned services (including the default browser), not those dynamic children; see the example's platform-specific workflow.

## Direct networking

Case study: having 2 rooms, every with max 5 users, they only need 10 ports to be forwarded so the range can be `59000-59009`.

![architecture](./architecture.svg)

All clients visit `https://neko-rooms/room-name/`. Once they logged in, they get one of the ports allocated where the RTC data flows. It happens inside SDP negotiation and is completly transparent to the clients. Therefore those ports needs to remain open, but they will never be actually visited directly in the browser.

`NEKO_ROOMS_NAT1TO1` must be the IP where the mentioned UDP ports are forwarded. If this setting is not present, it will get automatically servers public IP at start of every room that will be sent to clients.

## Connection timeout

Neko room loads but you don't see the screen and it gives you `connection timeout` or `disconnected error`? [Validate](https://neko.m1k1o.net/#/getting-started/troubleshooting?id=validate-udp-ports-reachability) that your UDP ports are reachable.

## path prefix

Room names are by default put to root directory. If you want to have custom path prefix, you can specify it using env variable:

```
NEKO_ROOMS_PATH_PREFIX=/test/
```

Now room will be available at `example.org/test/<room-name>` instead of `example.org/<room-name>`.

## using mux

When using mux, there will be allocated two ports per room: TCP and UDP port with the same number.

```
NEKO_ROOMS_MUX=true
```
