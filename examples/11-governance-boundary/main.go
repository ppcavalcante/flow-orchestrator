// Command 11-governance-boundary demonstrates M23's WithBoundary — a build-time
// governance primitive that refuses a topology in which the sink can be reached
// without first passing a verifier.
//
// The honest topology is a straight gate:
//
//	seed ──▶ doer ──▶ verify ──▶ accept
//
// WithBoundary(doer, verify, accept) declares "accept must not occur before
// verify": on every route the executor can take through the built graph, verify
// precedes accept. That is a Precedence(verify, accept) property scoped to
// CONTROL FLOW — a verifier-DOMINANCE check.
//
// The teaching point is the footgun the primitive catches. Add ONE stray edge —
// accept.DependsOn(doer) — and accept can now be reached straight from doer,
// bypassing verify. Without the boundary that mis-wire builds clean (the DAG is
// still acyclic). WITH the boundary declared, Build REFUSES it: an ErrValidation
// naming the offending root→sink path that reaches accept "without passing
// verifier". A governance mistake becomes a compile-shaped failure, not a
// silent production hole.
//
// This is a BUILD-TIME guard — no Execute is needed to see the rejection. The
// refused build IS the lesson, so main() does NOT treat it as an error; it
// treats a bypass that builds CLEAN as the defect.
//
// Run it:
//
//	go run ./examples/11-governance-boundary
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// Node names. The boundary is declared over three of them — the doer (the actor
// the governance rule is about), the verifier (the gate), and the sink (the
// privileged effect that must sit behind the gate).
const (
	nodeSeed   = "seed"
	nodeDoer   = "doer"
	nodeVerify = "verify"
	nodeAccept = "accept"
)

// keyAccepted records the terminal fact of the honest run, so the smoke test can
// assert the accepted topology actually executes to completion.
const keyAccepted = "accepted"

// buildBoundaryWorkflow wires seed→doer→verify→accept and DECLARES the
// (doer, verify, accept) boundary. When withBypass is true it also adds the
// footgun edge accept.DependsOn(doer), giving accept a route that skips verify.
//
// The boundary is validated in Build (which FromBuilder calls): with the bypass
// edge present, this function returns an ErrValidation naming the offending
// path; without it, it returns a runnable *Workflow. The smoke test builds the
// identical graph both ways.
func buildBoundaryWorkflow(store workflow.WorkflowStore, withBypass bool) (*workflow.Workflow, error) {
	noop := func(name string) func(context.Context, *workflow.WorkflowData) error {
		return func(_ context.Context, _ *workflow.WorkflowData) error {
			fmt.Printf("  %s ran\n", name)
			return nil
		}
	}

	b := workflow.NewWorkflowBuilder().
		WithWorkflowID("governance-boundary").
		WithStore(store)

	b.AddStartNode(nodeSeed).WithActionFunc(noop(nodeSeed))
	b.AddNode(nodeDoer).WithActionFunc(noop(nodeDoer)).DependsOn(nodeSeed)
	b.AddNode(nodeVerify).WithActionFunc(noop(nodeVerify)).DependsOn(nodeDoer)

	accept := b.AddNode(nodeAccept).DependsOn(nodeVerify)
	accept.WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set(keyAccepted, true)
		fmt.Printf("  %s ran (recorded accepted=true)\n", nodeAccept)
		return nil
	})

	if withBypass {
		// THE FOOTGUN: accept can now be reached directly from doer, skipping
		// verify. Structurally legal (still a DAG) — the boundary is what makes
		// it a Build-time error.
		accept.DependsOn(nodeDoer)
	}

	// Declare the governance rule: verify must precede accept on every route.
	b.WithBoundary(nodeDoer, nodeVerify, nodeAccept)

	// FromBuilder validates the graph — including the boundary — and returns a
	// store-backed *Workflow, or the validation error.
	return workflow.FromBuilder(b)
}

func run() error {
	// --- 1. The honest topology builds clean AND runs to completion ----------
	fmt.Println("honest topology (seed→doer→verify→accept):")
	store := workflow.NewInMemoryStore()
	wf, err := buildBoundaryWorkflow(store, false)
	if err != nil {
		// The honest, correctly-gated topology MUST build. A refusal here would
		// mean the guard over-rejects — a real defect in the example.
		return fmt.Errorf("honest topology was refused at Build (guard over-rejects): %w", err)
	}
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("honest topology failed to execute: %w", err)
	}
	data, err := store.Load("governance-boundary")
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	accepted, _ := data.GetBool(keyAccepted)
	fmt.Printf("  → built clean and ran: accepted=%v\n\n", accepted)

	// --- 2. The bypass footgun is REFUSED at Build — the lesson --------------
	// A refused Build here is the SUCCESS this example teaches, so it is NOT an
	// error to report. The defect would be the opposite: a bypass that slips
	// through. We assert the guard fired, with the right error and message.
	fmt.Println("bypass topology (adds accept.DependsOn(doer), skipping verify):")
	_, err = buildBoundaryWorkflow(workflow.NewInMemoryStore(), true)
	if err == nil {
		return fmt.Errorf("bypass footgun built clean — the boundary guard did NOT fire")
	}
	if !errors.Is(err, workflow.ErrValidation) {
		return fmt.Errorf("bypass was refused, but not as an ErrValidation: %w", err)
	}
	if !strings.Contains(err.Error(), "without passing verifier") {
		return fmt.Errorf("bypass refusal did not name the dominance violation: %w", err)
	}
	fmt.Printf("  → refused at Build, as designed:\n     %v\n", err)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("11-governance-boundary: %v", err)
	}
}
