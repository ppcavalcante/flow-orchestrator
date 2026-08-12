package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// defDigestKey is the reserved WorkflowData key under which the executor stamps a
// DAG's DefinitionDigest into the checkpoint, so a later resume can detect a
// changed graph definition (AUD-010). Like boundariesKey it is engine metadata
// living in the consumer key namespace on an interim basis; the M24 data-plane
// split (AUD-018) moves all such keys out of the consumer map together.
const defDigestKey = "__def_digest__"

// DefinitionDigest returns a stable, deterministic hex digest of this DAG's
// STRUCTURAL definition (AUD-010 / C-07): the node set, the dependency edges, and
// per node the retry/timeout/continue-on-error policy, compensation presence,
// suspendability, and action + compensation KIND — plus the boundary declarations.
//
// It is order-independent (nodes and dependencies are sorted), so two builds of
// the same definition yield the same digest, and any of these changes yields a
// different one: adding/removing a node, changing an edge, a retry/timeout/CoE
// policy, a compensation, a boundary, an action type, or suspendability.
//
// LIMIT, stated because a durable guard's blind spot must be published: %T
// captures an action's concrete TYPE, not its runtime BEHAVIOUR — a Go closure is
// opaque, so swapping the body of an ActionFunc without changing its type is
// INVISIBLE to this digest. A consumer who changes what an action DOES without
// changing its type should pair the digest with an explicit definition/semantic
// version. This is the "graph identity, not code shape" boundary from DEC-M9.
func (d *DAG) DefinitionDigest() string {
	h := sha256.New()

	names := make([]string, 0, len(d.nodes))
	for name := range d.nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		n := d.nodes[name]
		deps := make([]string, 0, len(n.dependsOn))
		for _, dep := range n.dependsOn {
			deps = append(deps, dep.name)
		}
		sort.Strings(deps)
		// One canonical NUL-delimited line per node. int64(timeout) normalizes the
		// Duration; %T on a nil compensation renders "<nil>", which is itself the
		// "no compensation" signal (also carried explicitly by comp=%t).
		//nolint:errcheck // sha256 hash.Hash.Write never returns an error (documented)
		fmt.Fprintf(h, "node\x00%s\x00deps=%s\x00retry=%d\x00timeout=%d\x00coe=%t\x00comp=%t\x00susp=%t\x00act=%T\x00compact=%T\n",
			name, strings.Join(deps, ","), n.retryCount, int64(n.timeout),
			n.continueOnError, n.compensation != nil, n.suspendable, n.action, n.compensation)
	}

	decls := make([]string, 0, len(d.boundaries))
	for _, b := range d.boundaries {
		decls = append(decls, b.doer+"\x00"+b.verifier+"\x00"+b.sink)
	}
	sort.Strings(decls)
	for _, decl := range decls {
		fmt.Fprintf(h, "boundary\x00%s\n", decl) //nolint:errcheck // sha256 hash.Hash.Write never returns an error
	}

	return hex.EncodeToString(h.Sum(nil))
}

// shortDigest renders a readable prefix of a hex digest for error messages.
func shortDigest(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
