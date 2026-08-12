# Capstone — document processing pipeline

The full-capacity showcase: one realistic, durable document-processing workflow
that composes several of the library's capabilities on a **SQLite** store.

```
seed ─▶ ocr-pages (FAN-OUT over N pages) ─▶ route (CHOICE by doc type)
          image ─▶ transcode ┐
          text  ─▶ index-arm ├─▶ route-merge ─▶ index (SUB-WORKFLOW)
          other ─▶ skip-arm  ┘                      │
                                                     ▼
                              approval (APPROVAL gate) ─▶ moderation-cleared
                                (WAIT-FOR-SIGNAL) ─▶ publish
```

## Run it

```sh
GOTOOLCHAIN=local go run ./examples/capstone-document-pipeline
```

```
result: published=true
effects: ocr=3 transcode=1 index=1 publish=1
```

## Test it

```sh
GOTOOLCHAIN=local go test ./examples/capstone-document-pipeline/ -count=1
```

The test drives the pipeline to its published terminal and asserts the per-stage
effects. It uses a real SQLite store and a park/resume driver loop, so it is
guarded behind `testing.Short()` — `go test -short` skips it.

## The capabilities, in one graph

| Stage | Capability | Focused example |
|-------|-----------|-----------------|
| `ocr-pages` | dynamic **fan-out** — page count discovered at run time, one OCR branch per page, typed per-branch results collected into `page-ocr[i]` | 05 |
| `route` + `route-merge` | a **choice** that routes by document type, reconverged with a merge (a direct multi-branch dependency would be an "unstructured reconvergence" build error) | 04 |
| `index` | a **sub-workflow** — the document is indexed as a distinct child run with its own journal, its scalar result written back into the parent | 08 |
| `approval` | an **approval gate** that parks the run until a decision is delivered, correlated by the AUD-025 nonce read straight from the store | 07 |
| `moderation-cleared` | a downstream **wait-for-signal**, all on a durable **SQLite** store | 03, 07 |

## The driving model — parks and the store-only driver

The run **parks twice**: at the approval and at the moderation wait. A parked
`Execute` returns `workflow.ErrSuspended` — a *success* arm ("suspend is a crash
you chose"), not a failure. So the host loops, re-driving `Execute` until the run
completes.

The approval and the moderation signal are delivered by a **background driver
that holds only the store — never the `*Workflow`**. That is the realistic
shape: the thing that approves a run is usually not the thing that drives it (a
dispatcher, a signal pump, a competing-consumer worker). It reads the approval
correlation nonce with `workflow.ApprovalNonceFromStore(store, id, "approval")` —
which derives the nonce from the digest the executor already stamped into the
parked state, so no graph rebuild is needed — and delivers a matching
`ApproveSignal`. A stale or mis-correlated approval would be inert.

## What this capstone trims

This is a teaching capstone, not the production rig. It deliberately leaves the
following to their own focused examples so the composition stays readable:
retries and the error taxonomy (02), crash-kill/resume (03), the compensation
saga (06), competing-consumer dispatch across multiple worker processes (09),
and cron scheduling with concurrency caps (10). Each is a single capability shown
in isolation; this pipeline shows the subset above working *together*.
