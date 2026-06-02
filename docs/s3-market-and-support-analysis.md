# S3 Service — Market Analysis & Support Landscape

**Version:** 1.0
**Status:** Draft
**Last Updated:** 2026-05-26
**Context:** Companion to `seaweedfs-s3-service-standard.md`, `storaged-design.md`, `s3-database-schema-design.md`

---

## Table of Contents

1. [Market Pain Points](#1-market-pain-points)
2. [Most Common Support Ticket Categories](#2-most-common-support-ticket-categories)
3. [Stack-Specific Risk Areas](#3-stack-specific-risk-areas)
4. [Competitive Positioning](#4-competitive-positioning)

---

## 1. Market Pain Points

These are the dominant frustrations reported by customers of S3 providers across the market, ordered by frequency and severity.

### 1.1 Egress Fees — #1 Pain Point Industry-Wide

Unpredictable egress costs are the single biggest complaint across all S3 providers, affecting an estimated 90% of cloud customers. Egress fees can exceed storage costs by 3–5x in data-intensive workloads, making ROI calculations extremely difficult. At scale (e.g. 10TB/month of internet egress on AWS), transfer fees alone can exceed the storage bill.

The EU Data Act, applicable from September 2025, mandates the removal of switching fees including egress charges by January 2027 — signaling that regulatory pressure is now aligning with customer sentiment.

**Implication for our stack:** Our egress quota is tracked and enforced via `bandwidth_monthly`. How we *price and communicate* egress costs to customers upfront will be a major differentiator. Predictable, clearly-communicated egress pricing — even if not zero — is better received than opaque or surprise billing.

### 1.2 Pricing Opacity & Bill Shock

Even customers who accept variable pricing dislike not being able to forecast their bill. Complaints center on "overhead charges that feel inflated" and cost forecasts that are inaccurate. This is distinct from egress — it's about visibility into what is being charged and why.

**Implication for our stack:** Customers currently have no self-serve usage dashboard. Usage data lives in PostgreSQL and is surfaced only via cron emails at 80% and 100% thresholds. A read-only usage portal would significantly reduce inbound billing questions.

### 1.3 Vendor Lock-in

Vendor lock-in is a top concern for over 70% of enterprise IT leaders. The two mechanisms customers complain about are proprietary APIs (which make migration expensive technically) and punitive egress fees (which make migration expensive financially).

**Our position:** SeaweedFS exposes a standard S3-compatible API. Customers can migrate away using any standard S3 tool without code rewrites. This is a genuine differentiator that should be stated explicitly in onboarding materials.

### 1.4 Support Quality

Smaller S3 providers (Backblaze, Wasabi) frequently receive complaints about canned support responses and agents who deflect to the other party in integration disputes. Customers feel passed around rather than helped.

**Our position:** Operating a self-hosted, full-stack service means support staff can see the actual logs, query the actual database, and diagnose the actual issue — without escalating to an upstream vendor. This is a meaningful quality advantage that should be a stated part of the service proposition.

### 1.5 Unexpected Throttling & Account Suspension

Wasabi in particular is criticized for suspending accounts when egress exceeds monthly storage without clear prior warning. Customers discover the policy only after being hit by it.

**Our position:** Our enforcement model sends warning emails at 80% usage before any blocking occurs, and blocks via Nginx (returning a clear message) rather than silently failing. This is already more customer-friendly than Wasabi's approach — but the quality of the 80% and 100% notification emails matters enormously.

### 1.6 Reliability & Outage Risk

Hyperscaler outages (including a significant AWS event in October 2025 causing failed uploads and timeouts) have eroded some confidence in large providers. For smaller self-hosted providers the concern flips: customers worry about the provider's infrastructure maturity rather than the hyperscaler's resilience.

**Our position:** Single-server deployments carry real data loss risk. This must be explicitly communicated in customer SLAs, not buried. When Server 2 is added and replication activates (`-defaultReplication=001`), this risk profile changes significantly and should be communicated to customers as an upgrade.

---

## 2. Most Common Support Ticket Categories

Based on market data and direct analysis of our stack's architecture, these are the expected support ticket drivers ordered by volume.

### Category 1 — Access Denied / Credentials (Highest Volume)

This is universally the #1 support category across every S3 provider. The S3 API returns `403 Access Denied` without explaining which specific permission is missing, making it opaque and frustrating for customers.

**Expected ticket types for our stack:**

- Wrong endpoint URL configured in client tool (not `s3.yourdomain.com`)
- Copied access key or secret key incorrectly during setup
- Bucket name doesn't match the customer's slug prefix — our IAM pattern `Read:customer-a-*` silently denies access to buckets named outside the pattern, with no helpful error
- Customer trying to access another tenant's bucket and receiving a confusing 403 instead of 404
- **Quota-blocked customers** — the most avoidable ticket source. A customer gets blocked by the quota cron, doesn't understand why, and opens a ticket assuming their credentials are broken. They see `403` and have no context. This will be a significant driver unless block notification emails are explicit, timely, and actionable.

**Mitigation:** The block email must clearly state: (1) which quota was exceeded, (2) current usage vs limit, (3) exactly what is blocked, and (4) how to get unblocked. Ambiguity here generates the most avoidable tickets.

### Category 2 — Billing & Quota Confusion

Customers frequently contest or misunderstand their usage figures, even when the numbers are accurate.

**Expected ticket types for our stack:**

- "Why am I blocked? I thought I had 100GB and I'm at 95GB" — customer doesn't account for the up-to-15-minute enforcement lag allowing temporary overage before the check runs
- Confusion about monthly egress resets — customers who upload heavily at end of month and get blocked early the next month
- "I deleted a lot of files, why is my storage still showing the same?" — SeaweedFS storage reclamation is not instant; the volume server reclaims space on compaction, not immediately on delete
- Disputes between the egress figure calculated by our Nginx log parser vs what their S3 client tool reports (different counting methods, e.g. including/excluding headers)
- No self-serve way to check current usage between cron cycles

### Category 3 — Client & Integration Setup

Third-party S3 clients each have quirks that surface during onboarding and configuration.

**Expected ticket types for our stack:**

- Path-style vs virtual-hosted-style URL configuration (rclone, boto3, Cyberduck, Veeam, Duplicati, restic all have different defaults)
- Multipart upload timeouts on large files — Nginx `client_body_timeout 300s` may be too short for customers with slow uplinks uploading very large objects
- TLS certificate errors in older or misconfigured client tools
- Tools that probe for AWS-specific features (STS, bucket location, ACL headers) that SeaweedFS does not implement identically to AWS

**Mitigation:** A client setup guide covering the 5–6 most common tools (rclone, boto3/Python, Cyberduck, Veeam, restic, aws-cli) with exact config snippets would eliminate a large share of onboarding tickets.

### Category 4 — Performance & Slowness

Customers rarely distinguish between a throughput problem and a latency problem. Tickets arrive as "it's slow."

**Expected ticket types for our stack:**

- RAID contention between NFS VM disk traffic and S3 object traffic — both share the same server hardware in single-server deployments; heavy NFS activity during VM disk operations will impact S3 throughput
- Small object workloads performing poorly — many tiny files create excessive load on the SeaweedFS filer metadata layer
- Customer uploading without multipart, hitting throughput limits on large single-PUT operations
- GET performance degradation when Nginx is under high write concurrency (`proxy_buffering off` is correct for large uploads but can affect small-object GET latency)

### Category 5 — Data & Object Issues

- "My file disappeared" — almost always the customer deleted it from another session or tool
- Object listing inconsistency — customers who write then immediately list and don't see the object (consistency window)
- "My bucket says X objects but my tool shows Y" — pagination issues in their S3 client
- Requests to restore accidentally deleted data — **on single-server deployments without versioning enabled, this is not possible.** This must be in SLAs and onboarding documentation explicitly, or it becomes a damaging support interaction.

### Category 6 — Account Lifecycle

- **Lost credentials** — the secret key is generated once and delivered at provisioning. If the customer loses it, there is no recovery: only rotation, which requires reconfiguring all their client tools. This should be stated clearly at onboarding.
- Key rotation requests mid-operation that break running backup jobs
- Confusion about the bucket naming prefix requirement (`slug-*`) during initial bucket creation
- Offboarding / data retrieval questions when a customer wants to migrate away

---

## 3. Stack-Specific Risk Areas

These are issues unique to our architecture that could generate disproportionate support volume relative to our customer count.

### 3.1 The Blocking Flow UX Gap

The quota enforcement flow is technically correct but the customer experience of being blocked is a hard `403` with the message `"Storage quota exceeded. Please contact support."` Every blocked customer who did not adequately absorb the 80% warning email will open a ticket. The 80% email quality is therefore a direct lever on support volume.

**Recommendation:** Treat the 80% and 100% notification emails as a product, not a cron side-effect. They should include: current usage, quota limit, usage trend, estimated days until limit, and a clear call to action (upgrade, delete data, contact support).

### 3.2 No Self-Serve Usage Visibility

There is no customer-facing dashboard. Customers who want to know their current usage between cron cycles have no option except to open a ticket. This is a structural ticket generator that grows linearly with customer count.

**Recommendation:** A read-only usage endpoint or simple web page showing current storage, object count, and egress usage vs quota would eliminate a meaningful percentage of billing-related tickets.

### 3.3 Single-Server Data Loss Risk

The current architecture runs on one server. SeaweedFS is configured with `-defaultReplication=001` (ready for replication) but until Server 2 is added, all data is a single copy. A disk failure means data loss.

**Recommendation:** This must be documented in customer SLAs with explicit language. Customers who lose data and were not informed of the risk create support escalations that are disproportionately costly — reputationally and operationally.

### 3.4 Secret Key Non-Recovery

IAM secret keys are generated once at provisioning and never stored in plaintext after delivery. There is no "reveal secret key" function. Key loss forces rotation, which breaks all customer integrations until reconfigured.

**Recommendation:** Onboarding documentation must make this explicit. Consider whether a secure one-time credential reveal link (time-limited, logged) during the provisioning window would reduce this ticket type.

### 3.5 Bucket Naming Constraint Is Non-Obvious

The IAM permission model enforces bucket names matching `{slug}-*`. A customer who tries to create a bucket named anything outside this pattern will get a permissions error with no explanation from the API. This is a common onboarding trip point.

**Recommendation:** The provisioning welcome email and any documentation should lead with this constraint, with examples.

---

## 4. Competitive Positioning

Based on market pain points and our architecture, here is where we stand relative to the main S3 provider categories.

| Pain Point | AWS S3 | Wasabi / Backblaze | **Our Stack** |
|---|---|---|---|
| Egress pricing | High, opaque | Low/zero but with hidden rules | Defined quota, transparent |
| Billing predictability | Poor | Medium | Medium (no dashboard yet) |
| Vendor lock-in | High | Medium | Low — standard S3 API |
| Support quality | Automated/scaled | Canned responses | Full-stack visibility |
| Account suspension warning | None | None or minimal | 80% email warning before block |
| Data sovereignty | US/EU regions | US-centric | Self-hosted, your datacenter |
| Reliability (single-server) | 99.99%+ | 99.9%+ | Lower — single copy until Server 2 |
| Feature completeness | Very high | Medium | Medium — core S3 API only |

### Where We Win

- **Support quality** — we control the full stack and can diagnose any issue end-to-end
- **Data sovereignty** — data never leaves the customer's chosen datacenter
- **Pricing transparency** — soft quota enforcement with advance warning, no surprise bills
- **No lock-in** — standard S3 API, customers can migrate with any S3 tool

### Where We Need to Improve

- **Self-serve usage visibility** — no dashboard is a gap versus even basic competitors
- **Reliability** — single-server risk must be closed by adding Server 2 and activating replication
- **Onboarding documentation** — client setup guides and bucket naming rules need to be first-class, not afterthoughts
- **Notification email quality** — the 80% and block emails are the main UX surface between us and our customers; they must be clear and actionable

---

*End of Document*

---

> **Document Owner:** Infrastructure Team
> **Companion Documents:** `seaweedfs-s3-service-standard.md`, `storaged-design.md`, `s3-database-schema-design.md`
> **Repository:** `git@yourdomain.com:infra/s3-service-standard.git`
