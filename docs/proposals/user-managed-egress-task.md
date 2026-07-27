# Task: BYO infrastructure / user-managed egress proposal

## Goal

Produce a design proposal for `provider-gcp` that gives shoot owners more freedom over the network topology of their shoot — analogous to the "BYO subnet + user-managed egress" work happening in `provider-azure` and `provider-aws`.

The concrete outcome is a markdown design document at `docs/proposals/flexible-network-configuration.md` in this repo, ready for review. Do not write source code changes as part of this task — the proposal is the deliverable.

## Prior art

Study both before starting:

- **`provider-azure` proposal (in progress)** — branch `docs/user-managed-egress-proposal` at <https://github.com/kon-angelo/gardener-extension-provider-azure>. File: `docs/proposals/flexible-network-configuration.md`. Use this as the primary structural and stylistic template.
- **`provider-aws` PR** — <https://github.com/gardener/gardener-extension-provider-aws/pull/1741>. Introduces per-zone BYO subnet + BYO security group + skip-matrix in the reconciler. AWS's `docs/proposals/flexible-network-configuration.md` (on the PR's branch) is the multi-provider design reference.

Both approaches use a **derived mode**: no new `OutboundType`/`Mode` enum is added; the mode is inferred from the presence of optional BYO reference fields on `InfrastructureConfig.Networks`. Follow the same pattern in GCP unless there is a strong GCP-specific reason to deviate — but if you think there is, discuss it before committing to a divergent shape.

## Search areas

You are expected to explore these codebases and produce a proposal that is precise about what changes where (file:line references for anything you claim about current behavior).

### `provider-gcp` (this repo)

- `pkg/apis/gcp/types_infrastructure.go` + `pkg/apis/gcp/v1alpha1/types_infrastructure.go` — the current API surface and what BYO fields already exist.
- `pkg/apis/gcp/validation/` — validation entry points and existing rules.
- `pkg/controller/infrastructure/` — the reconciler (Terraform-based or flow-based? task ordering; how BYO VPC is handled today).
- `pkg/controller/controlplane/valuesprovider.go` + `charts/internal/cloud-provider-config/` — how the CCM cloud-config is produced.
- `pkg/controller/bastion/` — bastion controller's assumptions about the network.
- `docs/usage/` — existing user-facing docs about networking, VPC, Cloud NAT.

### Upstream Kubernetes cloud provider for GCP

- **`kubernetes/cloud-provider-gcp`** (<https://github.com/kubernetes/cloud-provider-gcp>) — the standalone out-of-tree CCM. What does its `cloud.conf` look like? Which fields matter for BYO scenarios? What does it do with routes, firewall rules, and load balancers?
- **`kubernetes/ingress-gce`** — the GCE ingress controller that ships in most GKE-like setups. Firewall rules and forwarding rules it programs on behalf of `Service type=LoadBalancer` and `Ingress` are directly relevant: any BYO-firewall-rule discussion has to reason about what ingress-gce mutates at runtime, exactly as the Azure proposal had to reason about the Azure CCM's `reconcileSecurityGroup` behavior.
- Older/embedded logic: some GCP cloud-provider behavior historically lived in `kubernetes/kubernetes` under `staging/src/k8s.io/legacy-cloud-providers/gce`. Check if any of that is still authoritative for behaviors relevant to the design.

### GCP-native references

Consult but do not over-index on GKE-specific vocabulary. Describe the design in terms of native GCP primitives (VPC network, subnetwork, VPC-scope firewall rules, custom routes, Cloud Router, Cloud NAT, Private Google Access, VPC peering, Shared VPC). Cross-link to GCP docs where useful.

## Deliverable

A single markdown document at `docs/proposals/flexible-network-configuration.md`, structured to mirror the Azure proposal's sections:

- Summary
- Motivation (Goals, Non-Goals)
- Background — today's egress/network behavior in `provider-gcp` (with file:line refs)
- Background — GCP's native network primitives (concise; assume the reader knows k8s but may not know GCP)
- Proposal
  - API changes (proposed new fields on `InfrastructureConfig` and `InfrastructureStatus`)
  - Derived mode (how the reconciler decides between managed and BYO)
  - Validation rules (both API-level and pre-flight runtime validation)
  - Reconciler behavior (task-gating matrix; one mermaid flowchart if it helps)
  - Status shape
  - Cloud-provider config (what changes in the CCM's `cloud.conf`)
  - Interaction with ingress-gce and any other in-cluster controllers that mutate GCP network resources — treat this like the Azure proposal's "NSG mutation contract" section
  - Bastion (if applicable in this mode)
  - Metadata tagging on BYO resources — matching Azure's approach with the `kubernetes.io/cluster/<technicalName>: shared` convention on whatever GCP resources make sense (probably firewall rules and routes; certainly not subnetworks since GCP subnets have limited tag semantics — verify)
- Configuration patterns — 2–3 concrete `InfrastructureConfig` examples
- Migration and immutability rules
- User responsibilities
- Deletion / teardown semantics
- Documentation plan
- Acceptance criteria — end-to-end scenarios grouped by regression / valid BYO / rejected configs / immutability / runtime invariants / deletion / metadata tagging. Follow the Azure proposal's table format exactly.
- Alternatives considered
- Open / Resolved questions
- Out of scope

## Interaction protocol

- **Ask clarifying questions before making major design commitments.** Examples of decisions to surface for input rather than resolve unilaterally:
  - Whether shared-VPC (host-project / service-project) is in v1 scope or deferred.
  - Whether GCP-equivalent "no egress" mode (no Cloud NAT, no default route to internet) is a distinct pattern or collapsed with "user-defined routing".
  - The API shape for BYO firewall rules — one field, per-purpose, or discovered from network tags?
  - Whether the GCP CCM's / ingress-gce's runtime firewall-rule mutation can be cleanly opted out per-Service (analogous to Azure's `service.beta.kubernetes.io/azure-disable-load-balancer-nsg-rule`).
- **Be precise about upstream references** — quote exact file paths and, where possible, line numbers, both for `provider-gcp` and for `cloud-provider-gcp` / `ingress-gce`.
- **No source-code changes.** The task is bounded to the proposal document.
- **No AWS / Azure / GKE branding** in the design body itself. Prior-art references belong in the "Alternatives considered" or "Prior art" section, framed as design comparisons rather than as authoritative sources.

## Reference commit

The current head of the Azure proposal branch (for pinning your reading) is at commit `013a94f0` on <https://github.com/kon-angelo/gardener-extension-provider-azure/tree/docs/user-managed-egress-proposal>. If that branch moves before you start, read whatever is at `docs/proposals/flexible-network-configuration.md` on that branch head.

## Deliverable branch

Push to a branch named `docs/user-managed-egress-proposal` on the operator's fork of `gardener/gardener-extension-provider-gcp`, mirroring the Azure workflow. Do not push to `origin` or open a PR without explicit approval.
