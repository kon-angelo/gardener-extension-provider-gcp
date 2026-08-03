# Flexible Network Configuration

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Background](#background)
  - [Current network topology](#current-network-topology)
  - [GCP network primitives](#gcp-network-primitives)
- [Proposal](#proposal)
  - [API changes](#api-changes)
  - [Derived mode](#derived-mode)
  - [Validation rules](#validation-rules)
  - [Reconciler behavior](#reconciler-behavior)
  - [Status shape](#status-shape)
  - [Cloud-provider config](#cloud-provider-config)
  - [Firewall-rule mutation contract](#firewall-rule-mutation-contract)
  - [Bastion](#bastion)
  - [Metadata labels on BYO resources](#metadata-labels-on-byo-resources)
  - [Single-stack IPv4 specifics](#single-stack-ipv4-specifics)
  - [Dual-stack specifics](#dual-stack-specifics)
- [Configuration patterns](#configuration-patterns)
- [Migration and immutability](#migration-and-immutability)
- [User responsibilities](#user-responsibilities)
- [Deletion / teardown](#deletion--teardown)
- [Documentation](#documentation)
- [Acceptance criteria](#acceptance-criteria)
- [Alternatives considered](#alternatives-considered)
- [Open questions](#open-questions)
- [Out of scope](#out-of-scope)

<!-- /toc -->

## Summary

Give shoot owners control over GCP network topology and egress by allowing them
to bring their own worker subnetwork inside their own VPC. In this mode the
infrastructure reconciler creates no network-layer resources — no worker
subnet, no Cloud Router, no Cloud NAT, no static firewall rules — and only
_references_ the pre-existing user resources. Egress becomes the user's
responsibility: their own Cloud NAT, a custom default route to a network
virtual appliance (NVA) or VPN/Interconnect, or no default route at all for
network-isolated shoots.

Firewall rules programmed at runtime by the cloud-controller-manager (for
`Service type=LoadBalancer`) and by `ingress-gce` (for dual-stack shoots) are
tag-scoped to worker VMs and additive; they compose safely with any firewall
policy the user has already installed on the BYO VPC.

Both single-stack IPv4 and dual-stack shoots are supported. The mode is
signaled by a single API field; there is no new outbound-type enum.

## Motivation

Today the extension supports BYO VPC and BYO Cloud Router, but always creates
the worker subnetwork, the Cloud Router NAT gateway, and four untargeted
firewall rules inside whichever VPC is referenced. This does not compose with
common enterprise topologies:

- **Central firewall egress.** All `0.0.0.0/0` from workers is sent to a
  central NVA or Azure/GCP-Firewall-equivalent in a hub project. Requires a
  user-owned default route on the worker subnet, incompatible with the
  Gardener-managed Cloud NAT that would otherwise SNAT the same traffic.
- **No egress at all.** Network-isolated shoots that terminate all traffic
  via Private Google Access + Private Service Connect. The user has no
  `0.0.0.0/0` route at all.
- **Fully user-owned VPC.** Enterprise platforms provision every subnet
  through a central team and hand shoot owners a subnet name. The four
  Gardener-created firewall rules — untargeted, applying to every VM in the
  VPC — are unacceptable in such shared VPCs.

### Goals

- Support BYO worker subnetwork inside a BYO VPC, for both IPv4-only and
  dual-stack shoots.
- Skip creating the Cloud Router, Cloud NAT, and static firewall rules when
  the user has taken over egress.
- Reuse the BYO worker subnet as the default subnet for internal Load
  Balancer forwarding-rule IP allocation, matching GCP's stock default.
- Zero breaking changes for existing shoots. Opting in is purely additive.

### Non-Goals

- **No new `OutboundType` enum.** Mode is derived from BYO field presence.
- **No BYO Cloud NAT or BYO Cloud Router API field.** In BYO mode Gardener
  owns neither the router nor the NAT gateway; users provision their own
  entirely out-of-band.
- **No BYO firewall-rule API field.** GCP firewall rules are per-Service
  objects and the CCM's runtime writes are tag-scoped, so BYO firewall
  behavior is achieved by _not creating_ rules on the reconciler side.
- **No BYO internal LB subnet.** In BYO mode the workers subnet is the LB
  subnet, matching GCP's stock default. Users who require partitioned LB IP
  space use the per-Service annotation
  `networking.gke.io/internal-load-balancer-subnet`, supported upstream.
- **No shared VPC (host / service project split).** Deferred.
- **No in-place transition** between managed and BYO on an existing shoot.

## Background

### Current network topology

The infrastructure reconciler (`pkg/controller/infrastructure/infraflow/`,
flow-based) creates the following GCP resources per shoot:

| Resource | Field / trigger | Managed name |
|---|---|---|
| Service Account | always | `<technicalID>` |
| VPC network | `Networks.VPC.Name` unset | `<technicalID>` |
| Worker subnet (`PurposeNodes`) | always | `<technicalID>-nodes` |
| Internal subnet (`PurposeInternal`) | `Networks.Internal` set | `<technicalID>-internal` |
| Services subnet (`PurposeServices`) | dual-stack only | `<technicalID>-services` |
| Cloud Router | `Networks.VPC.CloudRouter.Name` unset | `<technicalID>-cloud-router` |
| Cloud NAT | always, on the router | `cloud-nat` |
| NAT external IPs | `Networks.CloudNAT.NatIPNames` unset → auto | (auto) |
| Firewall rule `<technicalID>-allow-internal-access` | always | untargeted |
| Firewall rule `<technicalID>-allow-health-checks` | always | untargeted |
| Firewall rule `<technicalID>-allow-internal-access-ipv6` | dual-stack | untargeted |
| Firewall rule `<technicalID>-allow-health-checks-ipv6` | dual-stack | untargeted |

Two API-level BYO switches exist today: `Networks.VPC.Name` (BYO VPC by name)
and `Networks.VPC.CloudRouter.Name` (BYO Cloud Router by name). Neither
covers the subnet, NAT, or firewall-rule dimensions.

At runtime, the cloud-controller-manager and (for dual-stack shoots)
`ingress-gce` write additional resources into the same VPC:

| Resource | Written by | Cleaned up by |
|---|---|---|
| Custom routes `shoot--*` (per-node pod CIDR) | CCM route controller, IPv4 only | `ensureKubernetesRoutesDeleted` |
| Firewall rules `k8s-fw-*` per LB Service | CCM LB controller, tag-scoped | `ensureFirewallRulesDeleted` |
| Firewall rules `k8s-fw-l7-*`, forwarding rules, backends, health checks, NEGs | `ingress-gce` (dual-stack) | firewall cleanup filter matches; L7 resources not cleaned up by extension |

### GCP network primitives

For readers less familiar with GCP:

- A **VPC network** is a global object; **subnetworks** within it are regional
  and span all zones in a region.
- **Firewall rules** are VPC-scoped and can target VMs by network tag,
  service account, or (if unscoped) every VM in the VPC.
- **Cloud Router** hosts BGP sessions for Cloud VPN / Interconnect and hosts
  **Cloud NAT** gateways. A Cloud NAT gateway source-NATs egress traffic for
  specific subnets to a set of external IPs.
- **Routes** are VPC-scoped. The implicit `0.0.0.0/0 → default-internet-gateway`
  route is created with every VPC and can be overridden by higher-priority
  routes (lower priority number).
- **Alias IP ranges** are secondary CIDRs on a subnetwork, used by GCP's
  container-native pod IPAM.
- **Internal Load Balancer forwarding rules** allocate their IP from a
  referenced subnet's CIDR. The CCM defaults to the subnet named in
  `subnetwork-name` in its cloud config; Services may override with an
  annotation.

## Proposal

### Resource summary

The table below inventories every GCP resource the extension currently
creates or references, and states what happens to each one in BYO mode.
"Managed" rows are today's behavior, kept as-is; "BYO" rows are the change
introduced by this proposal.

**Per-shoot resources created by the infrastructure reconciler:**

| # | Resource | Managed mode | BYO mode |
|---|---|---|---|
| 1 | GCP IAM Service Account | created (`<technicalID>`) | unchanged — still created |
| 2 | VPC network | created (`<technicalID>`) unless `Networks.VPC.Name` set | required BYO reference (`Networks.VPC.Name`) — verify-only |
| 3 | Worker subnetwork (`PurposeNodes`) | created (`<technicalID>-nodes`) | required BYO reference (`Networks.SubnetNodes.Name`) — verify-only |
| 4 | Internal subnetwork (`PurposeInternal`) | created (`<technicalID>-internal`) if `Networks.Internal` set | forbidden; internal LB IPs allocate from the workers subnet via `subnetwork-name` fallback in `cloudprovider.conf` |
| 5 | Services subnetwork (`PurposeServices`) | created (`<technicalID>-services`), dual-stack only | dual-stack: required BYO reference (`Networks.SubnetServices.Name`) — verify-only. IPv4: N/A |
| 6 | Cloud Router | created (`<technicalID>-cloud-router`) unless `Networks.VPC.CloudRouter.Name` set | forbidden; user manages any router out-of-band |
| 7 | Cloud NAT gateway | created on the Cloud Router | not created; user manages egress out-of-band |
| 8 | Cloud NAT external IPs | auto-allocated, or referenced via `Networks.CloudNAT.NatIPNames` | N/A — no NAT |
| 9 | Firewall rule `<technicalID>-allow-internal-access` (IPv4) | created, untargeted (matches every VM in the VPC) | not created; user pre-provisions equivalent (recommended: tag-scoped to `<technicalID>`) |
| 10 | Firewall rule `<technicalID>-allow-health-checks` (IPv4) | created, untargeted | not created; user pre-provisions equivalent |
| 11 | Firewall rule `<technicalID>-allow-internal-access-ipv6` | created, untargeted, dual-stack only | dual-stack: not created; user pre-provisions equivalent |
| 12 | Firewall rule `<technicalID>-allow-health-checks-ipv6` | created, untargeted, dual-stack only | dual-stack: not created; user pre-provisions equivalent |
| 13 | IPv6 CIDR assignment on subnets | waited-for during reconcile, dual-stack only | N/A — user's subnets bring their own IPv6 CIDRs already |
| 14 | Alias-IP ranges on worker VMs (instance-scoped, dual-stack) | written per-VM by MCM using the workers subnet's secondary range `ipv4-pod-cidr` | dual-stack: written per-VM by MCM using the workers subnet's secondary range named by `SubnetNodes.PodSecondaryRangeName` |

**Per-Bastion resources (bastion controller):**

| # | Resource | Managed mode | BYO mode |
|---|---|---|---|
| 15 | Bastion VM + disk + NIC + external PIP | created; NIC attaches to workers subnet | unchanged; NIC attaches to BYO workers subnet |
| 16 | Firewall rule `<base>-allow-ssh` | created, tag-scoped to bastion VM | unchanged |
| 17 | Firewall rule `<base>-egress-worker` | created, tag-scoped to bastion VM | unchanged; reads `WorkersCIDR` from `InfrastructureStatus.Networks.Subnets[?Purpose=PurposeNodes]` instead of `Networks.Workers` |
| 18 | Firewall rule `<base>-deny-all` | created, tag-scoped to bastion VM | unchanged |

**Resources written at runtime by upstream controllers (not by this extension):**

| # | Resource | Written by | Managed mode | BYO mode |
|---|---|---|---|---|
| 19 | Custom routes `shoot--*` (per-node pod CIDR) | CCM route controller | written to Gardener-managed VPC (single-stack IPv4 only) | written to BYO VPC (single-stack IPv4 only) |
| 20 | Firewall rules `k8s-fw-*` (per LB Service) | CCM LB controller | tag-scoped to `<technicalID>` in Gardener-managed VPC | tag-scoped to `<technicalID>` in BYO VPC — composes with user's rules |
| 21 | Firewall rules `k8s-fw-l7-*`, forwarding rules, backends, health checks, NEGs, IPv6 LB frontends | `ingress-gce` (dual-stack only) | in Gardener-managed VPC + project | in BYO VPC + user's project |

**Cleanup on shoot delete:**

| # | What | Managed mode | BYO mode |
|---|---|---|---|
| Rows 1-14 | Resources created by the infrastructure reconciler | deleted by the reconciler | not created, nothing to delete |
| Rows 15-18 | Bastion resources | deleted by bastion controller on Bastion CR delete | unchanged |
| Row 19 | CCM custom routes | deleted by `ensureKubernetesRoutesDeleted` | deleted by `ensureKubernetesRoutesDeleted` — same filter, works against BYO VPC |
| Row 20 | CCM firewall rules | deleted by `ensureFirewallRulesDeleted` (filter: `k8s`-prefix + `TargetTag = <technicalID>`) | same filter, same behavior |
| Row 21 | `ingress-gce` resources | cleaned up by `ingress-gce` when owning `Ingress` / `Service` deleted; firewall rules also swept by `ensureFirewallRulesDeleted` | same, but user's project retains any leaked global resources if `ingress-gce` deletion did not complete cleanly |

### API changes

Two new optional fields on `NetworkConfig`
(`pkg/apis/gcp/types_infrastructure.go` + v1alpha1 mirror):

```go
// NetworkConfig holds information about the Kubernetes and infrastructure networks.
type NetworkConfig struct {
    // ... existing fields (VPC, CloudNAT, Internal, Worker, Workers, FlowLogs, MTU) ...

    // SubnetNodes is an optional reference to an already-existing worker subnetwork
    // inside the user-provided VPC. When set, the infrastructure reconciler creates
    // no network-layer resources in the user's VPC: it does not create or manage the
    // worker subnetwork, the services subnetwork, the Cloud Router, Cloud NAT, or
    // the static firewall rules. Firewall rules created at runtime by the
    // cloud-controller-manager for Service type=LoadBalancer and by ingress-gce for
    // dual-stack shoots are tag-scoped to worker VMs and continue to be
    // created/deleted normally.
    //
    // Requires Networks.VPC.Name to be set. Forbids Networks.VPC.CloudRouter,
    // Networks.Workers, Networks.Internal, Networks.CloudNAT, Networks.FlowLogs,
    // and Networks.MTU.
    // +optional
    SubnetNodes *SubnetReference `json:"subnetNodes,omitempty"`

    // SubnetServices is an optional reference to an already-existing subnetwork
    // used to allocate the IPv6 services CIDR (see the dual-stack specifics
    // section for details). Required together with SubnetNodes for dual-stack
    // shoots. Forbidden for single-stack IPv4 shoots and forbidden without
    // SubnetNodes.
    // +optional
    SubnetServices *SubnetReference `json:"subnetServices,omitempty"`
}

// SubnetReference references an existing subnetwork in an existing VPC.
type SubnetReference struct {
    // Name is the name of the subnetwork.
    Name string `json:"name"`

    // PodSecondaryRangeName is the name of the secondary IP range on the
    // referenced subnetwork carrying the IPv4 pod CIDR (used with alias-IP
    // pod IPAM). Required on SubnetNodes for dual-stack shoots. Ignored
    // (and rejected by validation) on SubnetNodes for single-stack IPv4
    // shoots and on SubnetServices in all cases.
    // +optional
    PodSecondaryRangeName *string `json:"podSecondaryRangeName,omitempty"`
}
```

No new status enum. Mode is inferred from `SubnetNodes` presence (see
[Derived mode](#derived-mode)).

**Summary of subnets and ranges the extension references** (BYO mode):

| Shoot networking | Worker subnetwork (`SubnetNodes`) | Services subnetwork (`SubnetServices`) |
|---|---|---|
| Single-stack IPv4 | Primary IPv4 range only. No secondary range. | Not used. |
| Dual-stack | Primary IPv4 range + secondary IPv4 range for pods (`PodSecondaryRangeName`) + external IPv6 `/64`. | External IPv6 `/64`. No secondary range. |

See [Dual-stack specifics](#dual-stack-specifics) for what each range is
used for.

### Derived mode

```go
// IsUserManagedEgress reports whether the shoot opts into BYO subnetworks and
// user-managed egress by bringing an existing worker subnetwork.
func (c *InfrastructureConfig) IsUserManagedEgress() bool {
    return c != nil && c.Networks.SubnetNodes != nil
}
```

The mode is signaled solely by `Networks.SubnetNodes`. No enum, no explicit
switch. Used in validation, reconciler task-gating, cloud-provider config
emission, and deletion.

### Validation rules

Added in `pkg/apis/gcp/validation/infrastructure.go`.

**API-level, when `Networks.SubnetNodes != nil`:**

| Rule | Reason |
|---|---|
| `Networks.VPC.Name` required | BYO subnet requires BYO VPC. |
| `Networks.VPC.CloudRouter` forbidden | Router is out of scope in BYO mode. |
| `Networks.Workers` (and deprecated `Networks.Worker`) forbidden | Worker CIDR is discovered from the actual subnet. |
| `Networks.Internal` forbidden | Internal LB IPs allocate from the workers subnet (see [Cloud-provider config](#cloud-provider-config)). |
| `Networks.CloudNAT` forbidden | User manages egress out-of-band. |
| `Networks.FlowLogs` forbidden | Flow logs are configured on the BYO subnet by the user. |
| `Networks.MTU` forbidden | MTU is a property of the BYO subnet. |
| `Networks.SubnetNodes.Name` non-empty, valid GCP subnet name | Standard field validation. |
| `Networks.SubnetNodes.PodSecondaryRangeName` forbidden for single-stack IPv4 | Not used (custom routes). |
| `Networks.SubnetNodes.PodSecondaryRangeName` required for dual-stack | Alias-IP pod IPAM depends on it. |
| `Networks.SubnetServices` required for dual-stack | IPv6 services CIDR is sliced from this subnet's IPv6 range. |
| `Networks.SubnetServices` forbidden for single-stack IPv4 | Only used for IPv6 services. |
| `Networks.SubnetServices` forbidden without `Networks.SubnetNodes` | The two are a matched pair. |

**Runtime (pre-flight, `pkg/controller/infrastructure/configvalidator.go`):**

| Rule | Reason |
|---|---|
| Referenced VPC exists (`GetNetwork`) | Fail fast with a clear error. |
| Referenced worker subnet exists in the VPC and in the shoot's region | Fail fast. |
| Worker subnet CIDR is a subset of `shoot.spec.networking.nodes` and non-overlapping with `shoot.spec.networking.{pods,services}` | Same guarantee as managed subnets today. |
| (dual-stack) worker subnet has `stackType: IPV4_IPV6` and an assigned external IPv6 CIDR | Required for IPv6 pods and LBs. |
| (dual-stack) secondary range named by `PodSecondaryRangeName` exists on the worker subnet, and its `ipCidrRange` equals `shoot.spec.networking.pods` | Alias-IP pod IPAM depends on exact CIDR match. |
| (dual-stack) referenced services subnet exists in the VPC and region, has `stackType: IPV4_IPV6`, and has an assigned external IPv6 CIDR | Services subnet is used only for its IPv6 range. |

**Immutability** (`ValidateInfrastructureConfigUpdate`):

- `Networks.SubnetNodes` cannot be added or removed after shoot creation.
- `Networks.SubnetNodes.Name`, `Networks.SubnetNodes.PodSecondaryRangeName`,
  and `Networks.SubnetServices.Name` are immutable once set.
- Existing VPC and CloudRouter immutability rules continue to apply.

### Reconciler behavior

Task-gating in `pkg/controller/infrastructure/infraflow/graph.go`:

| Task | BYO mode |
|---|---|
| `ensureServiceAccount` | unchanged |
| `ensureVPC` | verify-only via existing `ensureUserManagedVPC` |
| `ensureNodesSubnet` | **replaced** by `ensureUserManagedNodesSubnet` — verify existence, populate whiteboard, do not create or patch |
| `ensureInternalSubnet` | **skipped** |
| `ensureServicesSubnet` | **replaced** by `ensureUserManagedServicesSubnet` (dual-stack); skipped in IPv4 |
| `ensureCloudRouter` | **skipped** |
| `ensureCloudNAT` | **skipped** |
| `ensureIpAddresses` | **skipped** |
| `ensureFirewallRules` | **skipped** |
| `ensureIPv6CIDRs` | **skipped** (user's subnets bring their own IPv6 CIDRs) |
| `ensureAliasIpRanges` | dual-stack only, unchanged — writes are per-instance, not VPC-scoped |
| `ensureBYOResourceLabels` (new) | best-effort tag on BYO VPC + subnet(s) |

Delete graph: skip destruction of workers subnet, services subnet, VPC,
router, NAT, and the four static firewall rules. Run
`ensureKubernetesRoutesDeleted` and `ensureFirewallRulesDeleted` unchanged
— they remove the CCM's runtime writes from the BYO VPC on shoot delete.

### Status shape

`InfrastructureStatus.Networks` in BYO mode:

| Field | Value |
|---|---|
| `VPC.Name` | user-provided |
| `VPC.CloudRouter` | omitted |
| `Subnets[]` | one entry `{Purpose: PurposeNodes, Name: <BYO name>}`, plus `{Purpose: PurposeServices, Name: <BYO name>}` for dual-stack |
| `NatIPs[]` | empty |
| `IPFamilies` | as shoot networking |

`Infrastructure.Status.EgressCIDRs` is **nil** in BYO mode. Gardener has no
reliable way to know the user's egress IPs — they may come from a
user-provisioned Cloud NAT, an NVA, an on-prem gateway, or nothing at all.
Downstream consumers that rely on this field must handle the nil case;
documented in [User responsibilities](#user-responsibilities).

### Cloud-provider config

Changes in `pkg/controller/controlplane/valuesprovider.go`
(`getNetworkNames` at line 725) and the two cloud-provider-config templates.

`getNetworkNames` gains a fallback: when the `PurposeInternal` subnet is
absent — which is always the case in BYO mode — `subNetworkName` is
populated from the `PurposeNodes` subnet name. Managed-mode behavior is
untouched.

Resulting `cloudprovider.conf` in BYO mode:

```ini
[Global]
project-id="<projectID>"
network-name="<BYO VPC name>"
subnetwork-name="<BYO workers subnet name>"
multizone=true
local-zone="<region-zone>"
token-url=nil
node-tags="<shoot technicalID>"
```

The two upstream controllers (main CCM and `ingress-gce` for dual-stack) both
receive the BYO workers subnet as their `subnetwork-name`. Internal Load
Balancer forwarding rules default to allocating their IP from the workers
subnet's CIDR, matching GCP's stock default. Users who need to partition LB
IP space from worker IP space use the per-Service annotation
`networking.gke.io/internal-load-balancer-subnet`.

No `network-project-id` (shared VPC) is emitted; see
[Out of scope](#out-of-scope).

### Firewall-rule mutation contract

**What the infrastructure reconciler does in BYO mode**: nothing. The four
static allow rules (`<technicalID>-allow-internal-access`,
`<technicalID>-allow-health-checks`, and the IPv6 mirrors for dual-stack)
are not created. The user takes over responsibility for these ingress paths.

**What the CCM writes at runtime** (`kubernetes/cloud-provider-gcp` LB
controller):

| Trigger | Rule name | Target | Sources | Ports |
|---|---|---|---|---|
| `Service type=LoadBalancer` (external or internal) | `k8s-fw-<hash>-<svcName>` | `TargetTags = [<technicalID>]` | `spec.loadBalancerSourceRanges` or `0.0.0.0/0` | Service node ports |
| Same | `k8s-<hash>-hc` | Same | GCP LB proxy ranges | Health-check ports |
| Service delete | (removed) | | | |

All CCM writes are **tag-scoped to worker VMs**. They are additive with
respect to any firewall rules the user already has on the BYO VPC — GCP
firewall rules are OR-ed allow policies, so composition is safe.

**What `ingress-gce` writes** (dual-stack shoots only): additional firewall
rules with names `k8s-fw-l7-*`, likewise tag-scoped, plus L7-adjacent global
resources (forwarding rules, backend services, health checks, target
proxies, NEGs). See [Deletion / teardown](#deletion--teardown) for cleanup.

**What the user must pre-provision on the BYO VPC** before creating the shoot.
The user replaces the four static rules the reconciler stops creating with
equivalents scoped to their own workers. Recommended: tag-scope by the shoot's
technical ID so the rules compose cleanly with the CCM's runtime writes.

Single-stack IPv4:

```bash
# Equivalent of <technicalID>-allow-internal-access
gcloud compute firewall-rules create <name>-allow-internal \
  --project=<project> --network=<vpc> \
  --direction=INGRESS --priority=1000 \
  --source-ranges=<shoot.pods>,<shoot.nodes> \
  --target-tags=<technicalID> \
  --rules=icmp,ipip,tcp:1-65535,udp:1-65535

# Equivalent of <technicalID>-allow-health-checks
gcloud compute firewall-rules create <name>-allow-health-checks \
  --project=<project> --network=<vpc> \
  --direction=INGRESS --priority=1000 \
  --source-ranges=35.191.0.0/16,130.211.0.0/22,209.85.152.0/22,209.85.204.0/22 \
  --target-tags=<technicalID> \
  --rules=tcp:30000-32767,udp:30000-32767
```

Dual-stack additionally requires IPv6 mirrors:

```bash
# IPv6 intra-VPC
gcloud compute firewall-rules create <name>-allow-internal-ipv6 \
  --project=<project> --network=<vpc> \
  --direction=INGRESS --priority=1000 \
  --source-ranges=<workers subnet IPv6 CIDR>,<services subnet IPv6 CIDR> \
  --target-tags=<technicalID> \
  --rules=icmpv6,ipip,tcp:1-65535,udp:1-65535

# IPv6 GCP LB proxy ranges
gcloud compute firewall-rules create <name>-allow-health-checks-ipv6 \
  --project=<project> --network=<vpc> \
  --direction=INGRESS --priority=1000 \
  --source-ranges=2600:2d00:1:b029::/64,2600:2d00:1:1::/64,2600:1901:8001::/48 \
  --target-tags=<technicalID> \
  --rules=tcp:30000-32767,udp:30000-32767
```

The `<technicalID>` value is the shoot's Kubernetes technical ID (e.g.
`shoot--foo--bar`), which is the network tag every worker VM carries — see
`pkg/controller/worker/machines.go:232-237`. It is also the value emitted as
`node-tags` in the CCM's `cloudprovider.conf`.

Users MAY omit `--target-tags` to make the rules VPC-wide, matching today's
Gardener behavior exactly. Tag-scoping is strictly recommended for shared
BYO VPCs.

**Aside — why the recommendation is stronger than what Gardener does today.**
The four static rules created by the reconciler today are explicitly
untargeted — `NullFields` in `ensure_utils.go:264` nulls `TargetTags` and
`TargetServiceAccounts`, so every VM in the VPC is a valid destination. This
is workable in single-owner VPCs but permissive in shared VPCs where other
workloads exist. Every worker VM already carries the shoot's technical ID as
a network tag (`pkg/controller/worker/machines.go:232-237`), so the scope
information exists; today's static rules simply don't use it. Users
replacing these rules in BYO mode are recommended to close that gap. The CCM
runtime rules (`k8s-fw-*`) and bastion rules already tag-scope to the same
technical ID, so the composed rule set is consistent.

Tightening the reconciler's own static rules in managed mode to also
tag-scope is a natural follow-up but out of scope here — see [Out of
scope](#out-of-scope).

### Bastion

Bastion works as-is in BYO mode, with one plumbing correction:

- The bastion controller currently reads `WorkersCIDR` from
  `InfrastructureConfig.Networks.Workers` (`actuator.go:77-84`). In BYO mode
  that field is empty. It must instead read from
  `InfrastructureStatus.Networks.Subnets[?Purpose=PurposeNodes].CIDR` (or
  equivalent). Small refactor; no API change.
- The three bastion firewall rules (`<base>-allow-ssh`, `<base>-egress-worker`,
  `<base>-deny-all`) are tag-scoped to the bastion VM (`options.go:199-211`)
  and land on the BYO VPC as tightly-scoped rules that clean up on bastion
  delete.
- Gardener's GCP principal must have `compute.firewalls.{create,update,delete}`
  permission on the project owning the BYO VPC. If denied, bastion creation
  fails; the rest of the shoot is unaffected.

### Metadata labels on BYO resources

For observability, apply a single label on BYO resources that support labels:

- **VPC network**: `kubernetes-io-cluster-<technicalID> = shared`.
- **Worker subnetwork**: same key.
- **Services subnetwork** (dual-stack): same key.

Constraints:

- GCP labels are lowercase `[a-z0-9_-]{1,63}`. The shoot technical ID is
  normalised (double dashes collapsed, lowercased).
- Operations are best-effort: on IAM/Org-Policy denial, log a warning and
  continue. Labels are informational only; no code reads them back.
- Multiple shoots on shared BYO resources each add their own key; each shoot
  removes only its own key on deletion.

Implementation: new task `ensureBYOResourceLabels` in the reconcile graph,
mirror `removeBYOResourceLabels` in the delete graph.

### Single-stack IPv4 specifics

- Pod-to-pod traffic uses **custom routes** written to the BYO VPC by the
  CCM route controller (`--configure-cloud-routes=true`, one route per node,
  named `shoot--*`, next-hop instance = the node VM).
- No alias-IP secondary range is required on the BYO subnet.
  `Networks.SubnetNodes.PodSecondaryRangeName` and `Networks.SubnetServices`
  are both forbidden by validation.
- On shoot delete, `ensureKubernetesRoutesDeleted` removes all `shoot--*`
  routes from the BYO VPC.
- The default 400-routes-per-VPC quota applies. Large shoots on a densely
  populated shared BYO VPC may need the extended 1000-route quota (support
  ticket). Documented as a user-facing note.

### Dual-stack specifics

Dual-stack shoots require the user to pre-provision **two** subnetworks in
the BYO VPC (workers + services), with specific ranges configured on each.

**Subnetworks and ranges the extension references:**

| Subnetwork | Referenced by | Ranges required on the subnet |
|---|---|---|
| Worker subnetwork | `Networks.SubnetNodes.Name` | Primary IPv4 range (a subset of `shoot.spec.networking.nodes`), plus a secondary IPv4 range named by `Networks.SubnetNodes.PodSecondaryRangeName` with `ipCidrRange` equal to `shoot.spec.networking.pods`, plus an external IPv6 CIDR (`stackType: IPV4_IPV6`, `ipv6AccessType: EXTERNAL`; GCP assigns a `/64` when the subnet is created). |
| Services subnetwork | `Networks.SubnetServices.Name` | An external IPv6 CIDR (`stackType: IPV4_IPV6`, `ipv6AccessType: EXTERNAL`). No secondary range. Primary IPv4 range must exist (GCP requires one on every subnet) but is unused by Gardener. |

**How each range is used at runtime:**

| Range | Consumer | Purpose |
|---|---|---|
| Worker subnet primary IPv4 | GCE, CCM, bastion | Worker VM NIC IP allocation, node CIDR reporting, bastion NIC subnet. Also used as the default subnet for internal Load Balancer forwarding-rule IP allocation via `subnetwork-name` in `cloudprovider.conf`. |
| Worker subnet secondary IPv4 (`PodSecondaryRangeName`) | CCM Cloud Allocator, MCM | Pod IPv4 alias-IP allocation. Each worker VM receives a `/24` (or shoot-configured size) slice of this range. |
| Worker subnet IPv6 `/64` | CCM Cloud Allocator, ingress-gce | Each worker VM receives a `/96` slice from GCP; the Cloud Allocator sub-allocates `/112` per pod. IPv6 LB forwarding rules also allocate their VIP from this `/64`. |
| Services subnet IPv6 `/64` | Extension reconciler | The extension reads the assigned `/64` and slices out a `/108` for the IPv6 services CIDR (`shoot.spec.networking.services`, IPv6 family). This workaround exists because GCP does not permit reserving a specific IPv6 range — a subnet must be provisioned so GCP assigns one. |

**Component interactions in dual-stack:**

- `ingress-gce` is deployed in the seed control plane
  (`valuesprovider.go:484-491`, gated on `IsDualStackEnabled(...)`) to
  provision dual-stack (IPv4, IPv6) Load Balancers, which the CCM does not
  support alone.
- Both the CCM's `cloudprovider.conf` and the `cloud-provider-config-ingress-gce`
  ConfigMap reference the BYO worker subnetwork as `subnetwork-name`.
- No custom VPC routes are written by the CCM in dual-stack mode; pod-to-pod
  traffic uses alias IPs on the workers subnet.
- Firewall rules written by `ingress-gce` (`k8s-fw-l7-*`) are tag-scoped to
  the shoot technical ID and are cleaned up on shoot delete by the same
  filter that catches CCM-written rules.

**Sizing considerations (documented for users):**

- Worker subnet primary IPv4 range must be sized for worker VMs plus any
  internal Load Balancer forwarding-rule IPs (typically few).
- Worker subnet secondary IPv4 range must be at least the size implied by
  `shoot.spec.networking.pods` (the extension verifies equality).
- Worker subnet IPv6 `/64` is assigned by GCP at subnet creation; per-VM
  `/96` and per-pod `/112` allocations come from this range.
- Services subnet IPv6 `/64` is likewise GCP-assigned; only a `/108` slice
  is used.

## Configuration patterns

**Managed (unchanged, today's default):**

```yaml
apiVersion: gcp.provider.extensions.gardener.cloud/v1alpha1
kind: InfrastructureConfig
networks:
  workers: 10.250.0.0/16
```

**Managed subnetwork inside a BYO VPC (unchanged existing capability):**

```yaml
apiVersion: gcp.provider.extensions.gardener.cloud/v1alpha1
kind: InfrastructureConfig
networks:
  vpc:
    name: my-vpc
    cloudRouter:
      name: my-cloud-router
  workers: 10.250.0.0/16
```

**BYO subnetwork, single-stack IPv4:**

```yaml
apiVersion: gcp.provider.extensions.gardener.cloud/v1alpha1
kind: InfrastructureConfig
networks:
  vpc:
    name: my-vpc
  subnetNodes:
    name: my-workers
```

Shoot spec:

```yaml
spec:
  networking:
    ipFamilies: [IPv4]
    nodes:    10.100.0.0/16
    pods:     10.96.0.0/11
    services: 10.200.0.0/20
```

User pre-provisions `my-workers` in `my-vpc` with primary range containing
`10.100.0.0/16`, and any egress topology of their choice.

**BYO subnetwork, dual-stack:**

```yaml
apiVersion: gcp.provider.extensions.gardener.cloud/v1alpha1
kind: InfrastructureConfig
networks:
  vpc:
    name: my-vpc
  subnetNodes:
    name: my-workers
    podSecondaryRangeName: my-pods
  subnetServices:
    name: my-services
```

Shoot spec:

```yaml
spec:
  networking:
    ipFamilies: [IPv4, IPv6]
    nodes:    10.100.0.0/16
    pods:     10.96.0.0/11
    services: 10.200.0.0/20
```

User pre-provisions:

- `my-workers` — `stackType: IPV4_IPV6`, external `/64` IPv6 CIDR assigned,
  primary range `10.100.0.0/16`, secondary range `my-pods = 10.96.0.0/11`.
- `my-services` — `stackType: IPV4_IPV6`, external `/64` IPv6 CIDR assigned.

## Migration and immutability

- Existing managed shoots continue to work with no changes. `Networks.SubnetNodes`
  and `Networks.SubnetServices` are optional and additive.
- New shoots may opt into BYO mode at creation time.
- In-place transition between managed and BYO mode is forbidden. Once
  created with `SubnetNodes` set, the shoot stays in BYO mode; once created
  without, it stays managed. Enforced in `ValidateInfrastructureConfigUpdate`.
  Rationale: transitioning would recreate/delete the worker subnet, Cloud
  Router, Cloud NAT, and firewall rules while workloads are running —
  disruptive and unnecessary for initial delivery.

## User responsibilities

Before creating a BYO shoot the user MUST provide:

1. A VPC network.
2. A worker subnetwork inside that VPC in the shoot's region, with primary
   IPv4 CIDR that is a subset of `shoot.spec.networking.nodes` and
   non-overlapping with `shoot.spec.networking.{pods,services}`.
3. (Dual-stack only) The worker subnetwork additionally configured with:
   - `stackType: IPV4_IPV6` and `ipv6AccessType: EXTERNAL` — GCP assigns an
     external `/64` IPv6 CIDR at subnet creation.
   - A secondary IPv4 range with `ipCidrRange` **exactly equal** to
     `shoot.spec.networking.pods`. The name is arbitrary and passed into
     `Networks.SubnetNodes.PodSecondaryRangeName`.
4. (Dual-stack only) A separate services subnetwork in the same VPC and
   region, with `stackType: IPV4_IPV6` and `ipv6AccessType: EXTERNAL`.
   A primary IPv4 range is required by GCP (any small non-overlapping range
   will do; it is unused by Gardener) and no secondary range is needed. The
   extension slices a `/108` out of this subnet's `/64` for the IPv6
   services CIDR.
5. Firewall rules on the VPC allowing intra-shoot traffic and GCP LB
   health-check probes to worker VMs. See [Firewall-rule mutation
   contract](#firewall-rule-mutation-contract) for the concrete `gcloud`
   commands.
6. An egress topology of the user's choice: user-owned Cloud NAT on a
   user-owned Cloud Router, a `0.0.0.0/0` route to an NVA / VPN /
   Interconnect, or no default route for network-isolated shoots.
7. IAM permissions on the project owning the BYO VPC such that Gardener's
   GCP principal can create/update/delete firewall rules and (in
   single-stack IPv4) custom routes at runtime for the CCM and bastion
   controller.

The user MUST NOT:

- Rely on `shoot.status.provider.egressCIDRs` — it is nil in BYO mode.
- Run competing automation against the CCM's `shoot--*` routes or `k8s-fw-*`
  firewall rules; these are managed by the in-cluster CCM lifecycle.

## Deletion / teardown

- BYO VPC, worker subnetwork, services subnetwork (dual-stack) — never
  deleted by Gardener.
- Cloud Router / Cloud NAT — never created in BYO mode, never deleted.
- Four static infra firewall rules — never created in BYO mode, never
  deleted.
- CCM-authored `k8s-fw-*` firewall rules — deleted by
  `ensureFirewallRulesDeleted` (`ensure.go:658-663`), filter matches
  `k8s`-prefix + `TargetTag` = shoot technical ID.
- CCM-authored `shoot--*` custom routes (IPv4 only) — deleted by
  `ensureKubernetesRoutesDeleted` (`ensure.go:690-719`).
- `ingress-gce` L7 global resources (forwarding rules, backends, URL maps,
  target proxies, NEGs) — cleaned up by `ingress-gce` when the owning
  `Ingress` / `Service` is deleted. Users must ensure all such objects are
  deleted before shoot deletion.
- Observability labels on BYO VPC / subnets — removed best-effort; failure
  logs a warning and does not block deletion.

## Documentation

Two new user-facing documents:

1. `docs/usage/user-managed-egress.md` — step-by-step guide with `gcloud`
   recipes for pre-provisioning VPC + subnet(s) + firewall rules, worked
   examples for both egress patterns (user-owned Cloud NAT, NVA route,
   no-egress), and explicit warnings about the empty `EgressCIDRs` status.
2. A new subsection in `docs/usage/usage.md` cross-linking to (1).

The existing `docs/usage/ipv6.md` gains a section on dual-stack BYO
requirements (secondary range naming, services subnet).

## Acceptance criteria

Implementation must pass the following scenarios end-to-end.

### Group A — Regression

| ID | Configuration | Pass criteria |
|---|---|---|
| A1 | Managed default (no BYO fields) | Reconciles green; VPC, worker subnet, Cloud Router, Cloud NAT, four firewall rules all created; egress via Cloud NAT. |
| A2 | Managed subnet inside BYO VPC (`Networks.VPC.Name` + `CloudRouter.Name`) | Reconciles green; Gardener creates worker subnet + Cloud NAT on user's router + firewall rules inside BYO VPC. |
| A3 | Managed with `Networks.CloudNAT.NatIPNames` set | NAT uses user's static IPs; `EgressCIDRs` populated with `/32` entries. |
| A4 | Managed dual-stack | Reconciles green; workers + services subnets created; ingress-gce deployed; alias-IP pod IPAM works; IPv6 LBs functional. |

### Group B — Valid BYO configurations

| ID | Configuration | Pass criteria |
|---|---|---|
| B1 | BYO subnet, IPv4, user-owned Cloud NAT already attached | Shoot reconciles green; `cloudprovider.conf` contains `subnetwork-name = <BYO workers subnet>`; egress flows via user's NAT. |
| B2 | BYO subnet, IPv4, `0.0.0.0/0` route to NVA in user's VPC | Same reconcile; egress flows via NVA; per-node pod CIDR routes still written to VPC by CCM. |
| B3 | BYO subnet, IPv4, no default route (network-isolated) | Reconciles green; in-VPC traffic works; internet egress fails as expected. |
| B4 | BYO subnet + services subnet, dual-stack | Reconciles green; alias-IP pods functional; IPv6 services allocated from services subnet; ingress-gce creates IPv6 LBs. |
| B5 | BYO subnet, IPv4, multiple shoots sharing the same BYO VPC | Both reconcile; each shoot's CCM writes are tag-scoped and do not interfere. |

### Group C — Rejected configurations

| ID | Configuration | Pass criteria |
|---|---|---|
| C1 | `SubnetNodes` set, `VPC.Name` unset | API validation rejects. |
| C2 | `SubnetNodes` set + `VPC.CloudRouter` set | Rejected. |
| C3 | `SubnetNodes` set + `Networks.Workers` non-empty | Rejected. |
| C4 | `SubnetNodes` set + `Networks.Internal` set | Rejected. |
| C5 | `SubnetNodes` set + `Networks.CloudNAT` set | Rejected. |
| C6 | `SubnetNodes` set + `Networks.FlowLogs` set | Rejected. |
| C7 | `SubnetNodes` set + `Networks.MTU` set | Rejected. |
| C8 | IPv4 shoot + `SubnetNodes.PodSecondaryRangeName` set | Rejected. |
| C9 | Dual-stack shoot + `SubnetNodes.PodSecondaryRangeName` unset | Rejected. |
| C10 | Dual-stack shoot + `SubnetServices` unset | Rejected. |
| C11 | IPv4 shoot + `SubnetServices` set | Rejected. |
| C12 | `SubnetServices` set without `SubnetNodes` | Rejected. |
| C13 | BYO subnet name refers to a subnet that does not exist | Runtime validator rejects. |
| C14 | BYO subnet CIDR is not a subset of `shoot.spec.networking.nodes` | Runtime validator rejects. |
| C15 | Dual-stack BYO subnet with `stackType: IPV4_ONLY` | Runtime validator rejects. |
| C16 | Dual-stack BYO subnet where `PodSecondaryRangeName` does not exist on the subnet | Runtime validator rejects. |
| C17 | Dual-stack BYO subnet where secondary range CIDR mismatches `shoot.spec.networking.pods` | Runtime validator rejects. |

### Group D — Update / immutability

| ID | Change | Pass criteria |
|---|---|---|
| D1 | Managed shoot: attempt to add `SubnetNodes` | Rejected. |
| D2 | BYO shoot: attempt to remove `SubnetNodes` | Rejected. |
| D3 | BYO shoot: change `SubnetNodes.Name` | Rejected. |
| D4 | Dual-stack BYO shoot: change `SubnetNodes.PodSecondaryRangeName` or `SubnetServices.Name` | Rejected. |

### Group E — Runtime invariants

| ID | Assertion |
|---|---|
| E1 | Reconciler makes zero create/update calls against the BYO VPC, worker subnet, or services subnet. |
| E2 | No `<technicalID>-cloud-router` or `cloud-nat` resource exists in the shoot's project after reconcile. |
| E3 | No `<technicalID>-allow-internal-access` firewall rule exists after reconcile. |
| E4 | `cloudprovider.conf` contains `subnetwork-name = <BYO workers subnet>`. |
| E5 | Creating `Service type=LoadBalancer` (external) → CCM creates `k8s-fw-*` rules tag-scoped to the shoot technical ID; deleting → rules removed. |
| E6 | Creating `Service type=LoadBalancer` with `cloud.google.com/load-balancer-type: Internal` → forwarding rule allocates IP from workers subnet CIDR. |
| E7 | Creating `Service type=LoadBalancer` with `networking.gke.io/internal-load-balancer-subnet: my-lb-subnet` (where `my-lb-subnet` is a pre-existing user subnet) → forwarding rule allocates IP from `my-lb-subnet`. |
| E8 | Creating a `Bastion` resource → bastion VM created with NIC in BYO worker subnet; three tag-scoped firewall rules created; ingress SSH from `Bastion.Spec.Ingress` CIDRs succeeds. |
| E9 | `Infrastructure.Status.EgressCIDRs` is nil. |

### Group F — Deletion / teardown

| ID | Action | Pass criteria |
|---|---|---|
| F1 | Delete shoot after all `Service type=LoadBalancer` and `Ingress` objects deleted | BYO VPC, worker subnet, services subnet remain unchanged. Zero Gardener-created resources remain in the BYO VPC. |
| F2 | Delete shoot with an outstanding `Service type=LoadBalancer` | CCM removes the LB and its firewall rules before Gardener finalizes deletion. |
| F3 | Delete shoot where two shoots share the same BYO VPC | Deleted shoot's `k8s-fw-*` and `shoot--*` resources removed; other shoot's resources untouched. |

### Group G — Metadata labels

| ID | Action | Pass criteria |
|---|---|---|
| G1 | Reconcile BYO shoot with label-write permission | BYO VPC and subnet(s) carry `kubernetes-io-cluster-<technicalID>: shared`. Pre-existing labels preserved. |
| G2 | Second BYO shoot sharing the same VPC | Both shoots' labels coexist on the VPC. |
| G3 | Reconcile BYO shoot without label-write permission | Reconciles green; warning logged; no labels applied. |
| G4 | Delete BYO shoot | Own label removed; other shoots' labels preserved. |

## Alternatives considered

**Explicit `OutboundType` enum.** Rejected: presence-derived mode is
consistent with prior Gardener extension work, and a five-value enum where
four values are placeholder is worse than no enum.

**BYO firewall-rule API field.** Rejected: GCP firewall rules are
per-Service objects, not shared containers. The CCM's runtime writes are
already tag-scoped and additive. Adding a BYO firewall field would create
API surface with no functional benefit — the extension simply stops
creating its own untargeted allow rules and the user pre-provisions
whatever they want.

**BYO internal LB subnet as a separate field.** Rejected: the workers
subnet serves as the LB subnet by default (matches GCP stock behavior).
Users who need a specific LB subnet use the upstream per-Service
annotation `networking.gke.io/internal-load-balancer-subnet`.

**BYO Cloud NAT / Cloud Router as API fields.** Rejected: in BYO mode
Gardener owns neither the router nor the NAT gateway. If the user wants
NAT egress they provision their own out-of-band; Gardener has no reason
to reference either.

**Auto-discover the pod secondary range name from the subnet.** Rejected:
subnets may have multiple secondary ranges, and inference from CIDR
compatibility is fragile. Explicit `PodSecondaryRangeName` is one small
field with clear semantics.

**In-place transition between managed and BYO.** Rejected: recreates/deletes
the worker subnet, Cloud Router, Cloud NAT, PIPs, and firewall rules while
workloads are running. Deferred as future work.

## Open questions

- Confirm whether `secondary-range-name` in the CCM `cloudprovider.conf`
  is required for dual-stack BYO. Managed dual-stack works today without
  emitting it (`Node.Spec.PodCIDRs` carries the range). If BYO dual-stack
  works the same way, no emission is needed; if not, the value must be
  plumbed through from `SubnetNodes.PodSecondaryRangeName`.

## Out of scope

- Shared VPC (host / service project split). Would require emitting
  `network-project-id` in `cloudprovider.conf` and an IAM preflight
  against the host project. `Networks.VPC` can grow a `HostProjectID`
  field without a breaking rename.
- BYO firewall rules as an API field.
- BYO Cloud NAT or Cloud Router as API fields.
- BYO internal LB subnet as an API field.
- In-place transition between managed and BYO on an existing shoot.
- `--configure-cloud-routes=false` for overlay-CNI shoots. Would let the
  BYO VPC stay free of per-node routes.
- Proxy-only subnet (`purpose: REGIONAL_MANAGED_PROXY`) for Internal
  HTTP(S) Load Balancer via `ingress-gce`. Not created today in either
  managed or BYO mode; users provision it themselves if needed.
