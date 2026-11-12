<!-- SPDX-License-Identifier: Apache-2.0 -->

# Client addresses behind a proxy

The password login and the admin API slow down clients that keep failing, keyed by the client's
address (`internal/auth/ratelimit`). Behind an ingress or load balancer every request arrives from the
proxy, so without help all clients share one address: one client's failures can block the others.

## When to set it

Set `api.trustedProxies` (flag `--trusted-proxies`) when koinon-api is reached through an ingress
controller, a cloud load balancer or any other proxy that **sets or appends to `X-Forwarded-For` or
`Forwarded`**. List the networks the proxy connects from, for example the ingress controller's pod
addresses or the load balancer's source range. Never list a whole pod network or `0.0.0.0/0`: a
listed peer chooses the client address, so any pod that can reach the API port directly could escape the
limiters or lock a user out. Set `networkPolicy.apiFrom` too, so only the proxy reaches the port. Leave it empty when clients connect directly: then the headers are never
read, which is the default.

```yaml
api:
  trustedProxies: [203.0.113.10/32]
  trustedProxyHeader: x-forwarded-for   # or forwarded
```

## What it does

- The peer of the connection is not in the list: the headers are ignored and the peer is the client.
- The peer is in the list: the hops of the header are read from the right. A hop in the list is
  another proxy and is skipped; the first hop not in the list is the client. Hops to its left are
  never read (the client wrote them).
- A hop that is not an IP address (`unknown`, `_obfuscated`, garbage), or a `Forwarded` element with no
  `for`, ends the walk with the hop to its right. No header means the peer.
- Only `trustedProxyHeader` is read (`x-forwarded-for` by default, or `forwarded`); the other is the
  client's and is ignored, since a proxy that writes one leaves the other to the client.
- IPv6 clients are still grouped by /64 by the limiters.

A listed proxy that passes a client's `X-Forwarded-For` through unchanged lets that client pick its
address and escape the limiters; make sure every listed proxy appends to the header.
