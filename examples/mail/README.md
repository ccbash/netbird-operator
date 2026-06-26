# Exposing a mail server on many TCP ports under one hostname

A mail server publishes several services on **one hostname** but **many ports** —
e.g. `mail.example.com` on SMTP `25`, Submissions `465`, IMAPS `993`,
ManageSieve `4190`. This example shows how to expose that through NetBird.

## The model

A NetBird reverse-proxy service is **one domain + one listen port**, and NetBird
allows **only one service per domain**. Raw TCP/UDP connections carry no
hostname (no SNI), so the proxy routes them purely by **listen port**. Putting
those two facts together:

- Write **one `ReverseProxyService` per port**, all sharing the same
  `domain: mail.example.com` and `proxyCluster`, each with a distinct
  `listenPort` and `mode: tcp`.
- The operator registers each port under a distinct per-port **sibling**
  subdomain named after the backend port: `mail-<portName>.example.com` (e.g.
  `mail-smtp.example.com`, `mail-imaps.example.com`; the backend Service port's
  name, or its number when unnamed; surfaced in `status.serviceDomain`) so
  NetBird's one-service-per-domain rule is satisfied.
- Clients still connect to `mail.example.com:<port>`. Because the per-port names
  are **siblings** (under `example.com`, not `mail.example.com`), the registered
  NetBird custom domain — what derives the proxy cluster — must be the **parent**
  `example.com`.

Two reachability paths come for free and coexist:

- **Mesh-internal** — the `type=LoadBalancer` Service is advertised into the
  mesh as `mail-mail.<zone>`; NetBird peers reach **all** ports there directly,
  with real client IPs and no proxy.
- **Public** — external clients (and inbound `:25` from other mail servers) go
  through the reverse-proxy cluster via the per-port services above.

## Deploy

Set up the `Network`, `NetworkRouter` and `DNSZone` first (see
[`../expose`](../expose/README.md)), then:

```shell
kubectl create namespace mail
kubectl apply -f ./examples/mail/mail.yaml
```

Watch the synthesized domains appear:

```shell
kubectl -n mail get reverseproxyservice -o custom-columns=\
NAME:.metadata.name,HOST:.spec.domain,SERVICE_DOMAIN:.status.serviceDomain,READY:.status.conditions[?(@.type==\"Ready\")].status
```

## Requirements

- **The proxy cluster must allow custom ports** and actually listen on
  `25/465/993/4190` on its public ingress. Otherwise NetBird rejects the service
  with `custom ports not supported on cluster …`.
- **The parent domain (`example.com`) must derive the proxy cluster** — be the
  cluster address, a subdomain of it, or a registered NetBird custom domain
  pointing at it — because the per-port siblings (`mail-smtp.example.com`, …)
  derive the cluster through the parent. And `mail.example.com` needs **public
  DNS** (A/AAAA, plus the `MX` for inbound mail) pointing at the cluster ingress.
- **Set `backends[].port`** here: the backend Service has more than one port and
  an unset port defaults to the Service's *first* port — wrong for all but one of
  the mail ports. (Single-port backends can omit it.)
- **Use `mode: tcp`** (passthrough) even for implicit-TLS ports like `465`/`993`
  — the mail server terminates TLS itself, so its cert must be issued for
  `mail.example.com`. `mode: tls` is SNI-terminated at the proxy and so cannot
  share one hostname across ports.
- **`proxyProtocol: true`** makes the proxy prepend a PROXY protocol v2 header so
  the backend sees the real client IP (essential for SMTP — SPF, DNSBL,
  greylisting, fail2ban, `Received:` logging). The **mail server must be
  configured to accept PROXY protocol** from the proxy's source IP, or the
  connection is mis-parsed and the port breaks — enable trust on the backend
  *before* deploying these services. (Stalwart: set `proxyTrustedNetworks`;
  Postfix: `smtpd_upstream_proxy_protocol = haproxy`; Dovecot: `haproxy_trusted_networks`.)
- L4 services can't use `private: true`/`accessGroups` (the NetBird auto-ACL is
  HTTP-only). Restrict an L4 service with `accessRestrictions` (CIDR/geo) instead.
