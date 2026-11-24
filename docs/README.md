# Docs

## Networking modes

`NEKO_ROOMS_NETWORK_MODE=direct` remains the default; the per-room port/NAT guidance below describes **direct** mode. For the NAS's single ordinary HTTP(S)/WebSocket entry, select **`http`** and use the [HTTP desktop setup](../README.md#one-entry-httpwebsocket-desktop-nas-mode): no browser WebRTC, TURN service or second public media port. Its public room URL is `/room/<name>/`, with compiled assets under `/__aggregate/`; private Manager↔Neko media stays inside the room network. VP8/Opus, native token sessions and WebM/MSE playback are required; microphone/webcam upload is unsupported.

**`relay` is optional and different:** the [relay example](../examples/relay/) uses separate HTTPS + TURN/TLS listeners and private TURN allocations; it is not needed for `http`. Switching policy affects only new/explicitly recreated dynamic rooms, not running children or their Profile/Downloads data. TrueNAS activation/rollback uses official `app.update` with Custom App YAML and recreates all App-owned services (including the default browser), not those dynamic children; see the example's platform-specific lifecycle instructions.

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
