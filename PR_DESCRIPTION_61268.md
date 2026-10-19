# PR Description: Issue #61268, Azure SQL connections fail in ambient mode (Redirect connection policy)

## Summary
This PR documents why Azure SQL Database connections fail in ambient mode when the
server uses the **Redirect** connection policy, and the traffic must be registered
through a ServiceEntry bound to a waypoint. It covers the supported workarounds and
proposes a follow-up API change (port ranges on `ServiceEntry`) to close the gap
properly.

Refs #61268.

## Problem Statement
The reporter's setup:

- An ambient-mode workload in a namespace with `istio.io/use-waypoint`.
- A `ServiceEntry` for `<server>.database.windows.net:1433` (`MESH_EXTERNAL`,
  `resolution: DNS`, `protocol: TCP`) bound to the namespace waypoint.
- A Kubernetes NetworkPolicy that only lets egress leave through the waypoint, so
  unregistered destinations are blocked (REGISTRY_ONLY-style enforcement).

The initial connection to the gateway on port 1433 succeeds. The client then fails
during the pre-login handshake of the redirected connection:

```
Microsoft.Data.SqlClient.SqlException (0x80131904): A connection was successfully established
with the server, but then an error occurred during the pre-login handshake. (provider: TCP Provider,
error: 35 - An internal exception was caught)
 ---> System.Net.Sockets.SocketException (104): Connection reset by peer
...
ClientConnectionId before routing:99a503a3-...
Routing Destination:c0713f7a7c3b.tr13329.northeurope1-a.worker.database.windows.net,11030
```

## Root Cause
With the **Redirect** policy (Azure's default for clients inside Azure), the Azure SQL
gateway only handles the first part of the session. It then sends the client a
routing token, and the client opens a **second** TCP connection straight to the
database node:

- host: `<id>.<tenant-ring>.<region>.worker.database.windows.net`
- port: any port in **11000–11999**

The second connection is the one that fails, for two reasons that compound:

1. **The redirected destination isn't registered.** The worker hostname is dynamic
   and isn't covered by the `ServiceEntry`. ztunnel therefore treats it as
   unregistered traffic and sends it directly to the destination IP instead of
   through the waypoint. The reporter's NetworkPolicy blocks that path, which shows up
   as `Connection reset by peer`.
2. **A ServiceEntry can't describe the destination, even with a wildcard host or CIDR.**
   - `ServiceEntry.ports` only takes discrete port numbers. The redirected port can
     be any value from 11000 to 11999, and listing 1000 ports isn't practical.
   - A wildcard host (`*.worker.database.windows.net`) doesn't help plain TCP. The
     TDS pre-login exchange happens before TLS, so the proxy has no SNI or Host header
     to match on. Wildcard `DYNAMIC_DNS` entries only work for HTTP and for TLS with
     SNI.
   - CIDR `addresses` on a `resolution: NONE` ServiceEntry are already supported in
     ambient (`toNetworkAddressFromCidr` in
     `pilot/pkg/serviceregistry/ambient/helpers.go`). However, the waypoint builds
     filter chains only for the ports the ServiceEntry declares
     (`buildWaypointInternal` in `pilot/pkg/networking/core/listener_waypoint.go`
     iterates `svc.Ports`). A connection to an undeclared port such as 11030 matches
     no chain and is closed.

Closing connections to undeclared ports is the correct REGISTRY_ONLY behavior, so this
isn't a regression in istio/istio. What's missing is a way to *register* a port range.

## Why Azure Service Bus works but Azure SQL doesn't
Service Bus (AMQP over TLS on 5671/443) always connects to a fixed, SNI-bearing
endpoint, so a normal `ServiceEntry` covers it. Azure SQL is the unusual case: in
Redirect mode it moves the session to a dynamic host and port after the first
handshake.

## Workarounds available today
1. **Switch the Azure SQL server to the `Proxy` connection policy.** All traffic stays
   on `<server>.database.windows.net:1433`, which the existing ServiceEntry already
   covers. The cost is an extra hop through the Azure SQL gateway, so latency goes up.
2. **Keep Redirect and allow the worker range explicitly.** Register the regional
   Azure SQL IP ranges (Azure service tag `Sql.<Region>`) in a
   `resolution: NONE` ServiceEntry with CIDR `addresses`, **without** binding it to a
   waypoint. Then add a NetworkPolicy egress rule for those CIDRs on TCP
   11000–11999. Traffic remains limited to Azure SQL in that region instead of being
   fully open, but it bypasses waypoint L7/L4 policy for the redirected leg.
3. **Private Endpoint.** With Azure Private Link, connections to a private endpoint
   always use Proxy semantics on port 1433 against a private IP in the VNet. A plain
   ServiceEntry (or the endpoint's IP) covers it, and it avoids the public gateway hop
   that Proxy mode adds.

## Proposed follow-up (istio/api + istio/istio)
Add port-range support to `ServiceEntry` ports, for example:

```yaml
spec:
  hosts:
  - sql-workers.northeurope.azure.internal
  addresses:
  - 203.0.113.0/24         # placeholder; use the Sql.NorthEurope service-tag ranges
  location: MESH_EXTERNAL
  resolution: NONE
  ports:
  - name: tds
    number: 1433
    protocol: TCP
  - name: tds-redirect
    number: 11000
    endPort: 11999           # new, inclusive; TCP/opaque protocols only
    protocol: TCP
```

Implementation outline:
- **istio/api:** add `ServicePort.end_port` (optional, `end_port >= number`, valid
  only with `resolution: NONE` and TCP/opaque protocols). Validation rejects
  overlapping ranges and `targetPort` combined with a range.
- **Ambient control plane:** extend `workloadapi.Port` (or add a range form) so
  ztunnel can match a service VIP or CIDR on a port range.
- **Waypoint:** in `buildWaypointInternal`, emit one filter chain per range using a
  destination-port range matcher, with an `ORIGINAL_DST` passthrough cluster, so
  authorization policy still applies to the redirected leg.
- **Sidecar:** treat a range as a single passthrough chain on the outbound listener
  in REGISTRY_ONLY mode.

## Scope
- In scope: root-cause write-up, documented workarounds, design for port-range
  ServiceEntry ports.
- Out of scope for this PR: functional code changes. These depend on the istio/api
  change landing first.

## Risk Assessment
This PR itself has no runtime impact. The proposed follow-up only adds a new optional
field and keeps existing ServiceEntries unchanged. The main risk is that a broad
range combined with a broad CIDR effectively opens egress, so validation and docs
should push users toward narrow service-tag ranges.

## Validation Plan (for the follow-up)
- Unit tests: ServiceEntry validation for ranges; ambient `constructServiceEntries`
  output; waypoint listener golden files with range filter chains.
- Integration: ambient egress test with a ServiceEntry range, checking that a TCP
  connection to an in-range undeclared port succeeds through the waypoint and that an
  out-of-range port is rejected.
- Manual: Azure SQL in Redirect mode from an ambient workload behind a waypoint, with
  a REGISTRY_ONLY-style NetworkPolicy.

## Backward Compatibility
No API or behavior change in this PR. The follow-up field is additive and optional.

## Checklist
- [x] Root cause identified and documented
- [x] Workarounds documented
- [x] Follow-up design proposed
- [ ] istio/api port-range field
- [ ] Ambient/waypoint/sidecar implementation and tests
