package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeliverSignal_AcquiresBeforeMkdirAll names a consequence the other tests do not, over an
// ordering they DO enforce. It is a diagnostic, not an independent guard — see below, where the
// two claims that justified writing it are recorded as false.
//
// deliverSignalToDir's acquire/count/MkdirAll block carries TWO properties:
//
//	P1  acquire BEFORE MkdirAll  => "lock file absent implies no mailbox work ever happened"
//	P2  MkdirAll AFTER the count => "a refused delivery leaves nothing behind"
//
// P1 is what makes removeSignalDir's skip sound: Delete opens the lock file without O_CREATE and
// proceeds unlocked when it is absent, which is safe ONLY because a delivery creates that file
// before touching any directory.
//
// WHAT THIS TEST IS AND IS NOT — corrected, because the first version of this comment claimed
// "P1 has no other guard" and "every other test stays green under a P1-breaking change", and
// BOTH WERE FALSE. Measured: moving the acquire below MkdirAll reds three other tests
// (TestMailboxCap_2Proc, AckRacingAnInFlightRedelivery, HoldsUnderConcurrentDelivery).
//
// The reason is an implication I had not worked out. The three ordering constraints are
//
//	P-count  acquire < count      (the count must be taken under the lock)
//	P2       count   < MkdirAll   (refusal must precede directory creation)
//	P1       acquire < MkdirAll
//
// and P1 follows from P-count ∧ P2 by transitivity. Both of those have behavioural tests, so
// P1 CANNOT be broken while they pass, and this assertion can never be the only thing that reds.
//
// It is kept anyway, as a DIAGNOSTIC rather than an independent guard, and that is the honest
// justification: under a P1-breaking change the behavioural tests report a cap breach and say
// nothing about removeSignalDir's skip, so a reader fixes the count and never learns that Delete
// can now RemoveAll a mailbox out from under a live delivery. This test names that consequence.
// A misleading diagnostic sending the reader to the wrong place is a failure mode this phase has
// already paid for twice.
//
// It does NOT cover the case where the mailbox directory is created somewhere other than
// deliverSignalToDir; it only reads this one function.
//
// ASSERTED OVER SOURCE ORDER rather than over a schedule, deliberately. P1 is a statement about
// the ORDER OF TWO CALLS IN ONE FUNCTION; no interleaving can express it. The go/ast form is the
// same instrument this package already uses for guard-count parity.
func TestDeliverSignal_AcquiresBeforeMkdirAll(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "signal_store.go", nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "deliverSignalToDir" {
			fn = f
			break
		}
	}
	require.NotNil(t, fn, "deliverSignalToDir not found in signal_store.go — the sweep is broken, not the code")

	// Walk the body in source order and record the position of the first call to each.
	lockPos, mkdirPos := -1, -1
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident: // lockMailboxDir(...)
			if f.Name == "lockMailboxDir" && lockPos < 0 {
				lockPos = fset.Position(call.Pos()).Offset
			}
		case *ast.SelectorExpr: // os.MkdirAll(...)
			if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "os" && f.Sel.Name == "MkdirAll" && mkdirPos < 0 {
				mkdirPos = fset.Position(call.Pos()).Offset
			}
		}
		return true
	})

	// ANTI-VACUITY FIRST, and the ordering of these two checks is itself a lesson from this
	// phase: the guard-count parity instrument once reddened on genuinely-broken history with
	// the message "the sweep found nothing, the sweep is broken" because its substantive check
	// ran before its anti-vacuity one. An instrument that cannot find its subject must say so in
	// those words, or a future reader debugs the test instead of reading the finding.
	require.GreaterOrEqualf(t, lockPos, 0,
		"no call to lockMailboxDir found in deliverSignalToDir. THE SWEEP IS BROKEN, NOT THE CODE "+
			"— either the function stopped locking (which is a far larger finding than this test "+
			"is written to report) or the call was renamed and this assertion needs updating.")
	require.GreaterOrEqualf(t, mkdirPos, 0,
		"no call to os.MkdirAll found in deliverSignalToDir. THE SWEEP IS BROKEN, NOT THE CODE — "+
			"the mailbox directory is created somewhere this test cannot see, so it is no longer "+
			"checking the ordering it claims to check.")

	require.Lessf(t, lockPos, mkdirPos,
		"deliverSignalToDir calls os.MkdirAll BEFORE lockMailboxDir. That breaks P1 — 'lock file "+
			"absent implies no mailbox work ever happened' — which is the entire soundness of "+
			"removeSignalDir's skip: Delete opens the lock file without O_CREATE and, finding it "+
			"absent, proceeds WITHOUT the lock. With this ordering a delivery can sit between "+
			"MkdirAll and the acquire with the directory present and the lock file absent, and a "+
			"concurrent Delete will os.RemoveAll the mailbox out from under it. Other tests DO "+
			"red under this change (P1 follows from P-count and P2, both of which are tested) — "+
			"but they report it as a CAP BREACH and never mention the skip, so fixing what they "+
			"point at leaves this consequence undiscovered. That is what this message is for.")
}
