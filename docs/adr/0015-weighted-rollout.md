# ADR 0015 — Weight is a share of traffic, and the walk is an operator's decision

**Status:** accepted · 2026-09-02
**Extends:** [ADR 0014](0014-eval-gate.md)
**Implements:** the rollout half of `model-gateway-architecture.md` §5.7

## Context

The plan's reasoning is the one that matters: **a fine-tuned model's regression
is silent.** No errors, no timeouts, no rise in any counter — just worse
output. So an adapter does not get switched on. It enters the routing table at
weight 0, takes a small share, and climbs; rollback is snapshot version N−1.

Three pieces of that already existed. `RoutingKey` has carried
`(BaseModel, AdapterID)` since M1, so multi-LoRA was designed in rather than
retrofitted; the router already excluded weight-0 deployments from selection;
and usage events already carried the adapter id.

One piece did not, and it was the load-bearing one.

## The gap

**`Weight` gated serving but never proportioned traffic.** `Serving()` was
`Weight > 0` and nothing else read the field. A canary at weight 1 alongside a
base model at 99 was simply another candidate, ordered by health, price and
locality — and a freshly trained adapter is healthy, the same price and in the
same region as the base model it came from, so it would frequently score
*highest* and take everything.

The canary would have looked like it was working. That is the shape of failure
this whole module exists to prevent.

## Decision

**Weight decides who serves; score decides who is best.** They answer different
questions and the canary is exactly where they disagree. So selection draws the
head of the candidate list proportionally to weight, and leaves the tail in
score order — which keeps failover picking the best remaining deployment.

**Only when the weights differ.** A weight is a *relative* share, so candidates
that all carry the same one are an operator saying "treat these equally", and
equally then falls to the score — the system's own judgement about health,
price and locality. Drawing lots between equals would discard that and make
every routing decision unreproducible for nobody's benefit. A test asserts the
cost objective still picks the cheaper of two equal-weight deployments, which
is what caught this during implementation.

**The draw is weighted by weight × health.** The weight is a number an operator
set yesterday; health is what is happening now. A canary whose breaker is
half-open must not keep taking its full share on the strength of the former —
that is what makes walking the steps survivable.

**The winner is rotated to the front, not swapped.** Swapping would put
whatever it displaced into the winner's old position, scrambling the failover
order beneath it.

**Adapter deployments are derived, not stored.** An adapter is not a new
endpoint — it is the base model's deployment serving a second routing key,
which is what multi-LoRA *means*: one vLLM pod holds many adapters and loads
them by id. The repository copies the base deployment and changes only the key
and the weight, so that stays true by construction. A stored copy would drift
the moment someone repointed the base model at a new provider.

**An adapter at weight 0 stays in the table.** Being there is what makes
rollback a snapshot version rather than a redeployment. An aborted rollout
returns to 0 and remains, because an aborted rollout is exactly the thing
someone will want to find later.

**Each step is an operator's decision, not a timer's.** This is the one place
this module deliberately stops short of the plan. Without a health signal to
advance on, a rollout that promoted itself would promote a bad adapter just as
reliably as a good one — and the regression is silent, so nothing would notice.
The machinery for the walk is here; the automation waits for the signal that
makes it safe, which is shadow scoring.

## Why not the alternatives

**Weighted random over the whole list rather than just the head.** Loses the
failover order, which is the other thing the list is for.

**Weight as a filter — include the canary in 1% of selections, exclude it
otherwise.** Equivalent for two candidates and wrong for three: excluding a
candidate entirely also removes it as a failover target.

**Consistent hashing on a request attribute instead of a draw.** Gives a stable
per-user assignment, which is genuinely better for a UX-visible change. It is
worse here: a fine-tune regression is a quality distribution, and pinning the
same users to the canary means the sample never widens. Worth revisiting for
rollouts where per-user stability matters more than sampling.

**Advance on a timer with a dwell.** Tempting, and it promotes a broken adapter
on schedule. The honest version of automatic advancement needs the health
signal, and that is the next module rather than this one.

## Consequences

- Routing becomes non-deterministic when an operator has asked for a split.
  That is what asking for a split means, and `WithDraw` makes it assertable in
  a test rather than something to sample until convinced.
- `READY` is still terminal for the job's own state machine; the rollout is
  tracked separately as `rollout_step` and `rollout_weight`, so `-1` is "no
  rollout" and `0` is "in the table, taking nothing". Those are different
  situations and a client walking the steps needs to tell them apart.
- Shadow traffic — mirrored, response discarded, scored offline — is not here.
  It is what turns the operator-driven walk into an automatic one, and it is a
  data-plane execution change rather than a control-plane one.
