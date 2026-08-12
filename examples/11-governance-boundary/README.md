# 11 — Governance boundary (`WithBoundary`)

M23's `WithBoundary(doer, verifier, sink)` is a **build-time governance
primitive**: it refuses any topology in which the sink can be reached without
first passing the verifier. It is a *verifier-dominance* check — a
`Precedence(verifier, sink)` property scoped to **control flow**.

## Run it

```sh
GOTOOLCHAIN=local go run ./examples/11-governance-boundary
```

## What it shows

The honest topology is a straight gate:

```
seed ──▶ doer ──▶ verify ──▶ accept
```

`WithBoundary(doer, verify, accept)` declares "accept must not occur before
verify". That builds clean and runs to completion.

The teaching point is the footgun the primitive catches. Add **one** stray edge —
`accept.DependsOn(doer)` — and `accept` gains a route straight from `doer` that
skips `verify`:

```
seed ──▶ doer ──▶ verify ──▶ accept
              └───────────────▲   (the bypass)
```

Without the boundary, that mis-wire builds clean — the DAG is still acyclic.
**With** the boundary declared, `Build` refuses it:

```
invalid workflow: validation failed: boundary (doer, verify, accept):
seed -> doer -> accept reaches sink "accept" without passing verifier "verify"
```

A governance mistake becomes a compile-shaped failure — an `ErrValidation` that
names the concrete offending root→sink path — instead of a silent production
hole.

## Why the refusal is the success

This is a **build-time** guard: no `Execute` is needed to see the rejection. The
refused build **is** the lesson, so `main()` does not treat it as an error — it
treats a bypass that builds *clean* as the defect. The smoke test asserts both
arms:

- the honest topology builds and reaches `accepted=true`;
- the bypass topology is refused with `errors.Is(err, workflow.ErrValidation)`
  and a message containing `without passing verifier`.

## The scope, precisely

The boundary is a claim about the **executor's traversal** of the built graph:
"on every route, the sink does not occur before the verifier." It is *not* a
claim that the doer's effect cannot otherwise reach the sink, and it does not
defend against a consumer forging a node's status out of band — those are
separate channels. The one property it proves, it proves at Build.
