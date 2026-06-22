# Architecture Decision Records (ADRs)

This directory contains Architecture Decision Records (ADRs) for Flow Orchestrator.

## What is an ADR?

An Architecture Decision Record (ADR) is a document that captures an important architectural decision made along with its context and consequences.

## ADR Files

ADRs are numbered sequentially and named using the format `NNNN-title-with-dashes.md` where:
- `NNNN` is a four-digit number that is incremented for each new ADR
- `title-with-dashes` is a short title for the ADR, with spaces replaced by dashes

## ADR Template

```markdown
# NNNN. Title of the ADR

## Status

[Proposed, Accepted, Superseded, etc.]

## Context

[Describe the context and problem statement, e.g., in free form using two to three sentences or bullet points.]

## Decision

[Describe the decision that was made.]

## Consequences

[Describe the resulting context after applying the decision.]

## Alternatives Considered

[Describe the alternatives that were considered and why they were not chosen.]

## References

[Optional: Include any references to other documents, articles, etc.]
```

## Current ADRs

| ADR | Title | Status | Milestone |
|---|---|---|---|
| [0001](0001-flatbuffers-load-trust-contract.md) | FlatBuffers Load trust contract | Accepted (superseded in part by 0008) | M1 |
| [0002](0002-integer-fidelity-contract.md) | Integer fidelity contract (int64-widen-additive) | Accepted | M2 |
| [0003](0003-m3-api-truth-surface-cleanup.md) | M3 scope — API Truth & Surface Cleanup | Accepted | M3 |
| [0004](0004-wire-maxconcurrency-default-16.md) | Wire MaxConcurrency end-to-end; default 16, bounded | Accepted | M3 |
| [0005](0005-delete-parallelnodeexecutor.md) | Delete the standalone ParallelNodeExecutor | Accepted | M3 |
| [0006](0006-remove-inert-intern-knobs.md) | Remove inert string-interning config knobs | Accepted | M3 |
| [0007](0007-error-taxonomy.md) | Error taxonomy — store sentinels + `%w` | Accepted | M3 |
| [0008](0008-layered-bounds-guard-trust-reratify.md) | Layered bounds guard for FlatBuffers Load; trust re-ratification | Accepted | M4 |

## How to Create a New ADR

1. Copy the template above
2. Create a new file with the next sequential number
3. Fill in the template
4. Add a link to the new ADR in this README file
5. Submit the ADR for review through a pull request 