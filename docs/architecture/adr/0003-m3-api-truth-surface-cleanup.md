# 0003. M3 scope — API Truth & Surface Cleanup

## Status

Accepted (milestone M3, 2026-06-15).

## Context

The path to "enterprise production grade" is a multi-milestone program. A 4-peer gap
assessment mapped the landscape into themes: (A) the public API tells the truth, (B)
untrusted-input safety, (C) gates actually enforce, (D) observability reaches a stack,
(E) docs + history honest, (F) architecture hygiene. The `pkg/workflow` surface had two
lying knobs (an inert `MaxConcurrency`, ignored intern-size config), ~341 exported decls
with no stability policy, and a thin error taxonomy. The user chose **aggressive cleanup
now** — break the public surface decisively while pre-1.0 to get it right before any
stability commitment.

## Decision

M3 = **Theme A (surface truthfulness)**, the first slice of an open-ended, leverage-driven
program. Clean the public surface — kill the lying knobs, prune the leaked-public exports,
add an error taxonomy, write STABILITY.md, backfill the CHANGELOG — **before** hardening (B),
gates (C), observability (D), or the broad docs sweep (E) build on it. Sequencing rationale:
do not fuzz, gate, instrument, or document a surface that is about to be ripped up.

## Consequences

- The public API becomes honest and dependable, de-risking the later themes.
- Deliberate breaking changes (inventoried in SURF-01, recorded in the CHANGELOG with
  migration notes), acceptable because pre-1.0 alpha.
- B/C/D/E/F are deferred and re-ranked from a fresh gap map next cycle, not dropped.

## Alternatives Considered

- **Trust & Hardening (Theme B) first** — hardens a surface about to be cleaned (rework);
  the urgent panic edge was already mitigated in M1.
- **Operability & trust signals (Theme C/D) first** — leaves the lying API surface
  untouched, which is the thing the user most signaled.

## References

- ADRs 0004–0007 record the four sub-decisions executed under this scope.
