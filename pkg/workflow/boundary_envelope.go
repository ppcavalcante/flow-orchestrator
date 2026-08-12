package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
)

// THE BOUNDARY PROJECTION IS INERT (DEC-M23-SEAM-INERT).
//
// The engine writes the validated declarations into WorkflowData once per FORWARD drive
// so that a snapshot of a run CARRIES what was declared. Forward, precisely (118-D3):
// projectBoundaries is called only from DAG.Execute, and executeLocked's IsRollingBack
// arm returns through finishRollback WITHOUT calling it. Nothing is lost -- a run that
// crashes into rollback already completed a forward drive that wrote the projection --
// but in a file about not overstating scope, the sentence should be the true one. NOTHING IN M23 READS IT BACK AS
// POLICY, and that is a design commitment rather than an accident of scheduling: the
// validated set is in the RUN-CONSTANCY class (dag.go), re-derived by build() from the
// rebuilt graph on every resume. A reader that took its policy from the store instead
// would be trusting a projection an operator can edit, and would make a stale snapshot
// authoritative over the graph.
//
// THE BAR-M23 ORACLE NOW EXISTS, and this paragraph is a statement rather than the
// requirement it was. It is barM23Oracle (cited by symbol, not by line -- lines drift):
// it quantifies over dag.boundaries, the run-constancy copy described above, and it
// never calls decodeBoundaryEnvelope. Phase 118b built it; F118-VER02-01, which
// recorded that 117's T10 was undelivered and unrecorded, closes there.
//
// 🔴 READ THE SCOPE BEFORE RELYING ON THIS. barM23Oracle is TEST-RESIDENT
// (bar_m23_oracle_test.go) and does not ship: M23 is a sealing milestone and the oracle
// needs unexported state, so it is not exported surface. And it evaluates BAR-M23
// CLAUSE 1 ONLY -- the V-written clause has no referent until M24. The oracle declares
// both facts in its own output rather than only in a doc comment, because phases
// 119-121 cite it as their exit gate and "oracle green" must not read as bar-green for
// a clause it cannot see.
//
// The history is kept because it is this milestone's defining defect in miniature
// (118-QA-01): this paragraph once read "The BAR-M23 oracle has a *DAG in hand and uses
// it" -- present tense, in a paragraph otherwise describing real run-constancy
// behaviour, and it was the ONLY mention of BAR-M23 in the non-test tree, so a reader
// had nothing to correct it against. Prose true of an intended design, restated as true
// of the system. The requirement form that replaced it was discharged by BUILDING the
// thing, which is the only way that ends.
//
// So this is a PROJECTION, in the read-model sense: write-only, downstream, and safe to
// be stale. The decoder below exists to pin the format's compatibility contract (and is
// exercised by tests), not to feed a decision.

// boundariesKey is the single namespaced key the projection is written under. One key
// holding one JSON STRING, which is the fanOutItemsKey precedent (fanout.go) and for
// the same measured reason: a slice does NOT round-trip uniformly across the four
// backends, a string does. SEAM-05 re-measured it for a namespaced key -- 13 key shapes
// x 4 backends, zero failures.
const boundariesKey = "__boundaries__"

// boundaryEnvelopeVersion is the version THIS build writes. It is an int64 and NEVER a
// bare int, and that is not style: SEAM-05 measured int -> int64 widening on all three
// durable stores while InMemoryStore keeps int. Since every in-package test runs on
// InMemoryStore, a bare int would compare equal in the whole test suite and differ only
// once persisted -- green here, skewed in production.
const boundaryEnvelopeVersion int64 = 1

// errBoundaryEnvelopeVersion is returned when a decoder meets a version it does not
// know. It is a distinct sentinel, not a string, so that "M24 extends rather than
// rewrites" is CHECKABLE rather than hoped: a test can assert on the refusal, and an
// M24 reader can branch on it. Wrapped alongside ErrValidation, which stays the domain.
var errBoundaryEnvelopeVersion = errors.New("unsupported boundary envelope version")

// boundaryEnvelope is the projected form. Field names are part of the persisted format:
// changing one is a version bump, not a rename.
type boundaryEnvelope struct {
	Version    int64                   `json:"version"`
	Boundaries []boundaryEnvelopeEntry `json:"boundaries"`
}

type boundaryEnvelopeEntry struct {
	Doer     string `json:"doer"`
	Verifier string `json:"verifier"`
	Sink     string `json:"sink"`
}

// encodeBoundaryEnvelope renders the declarations as the JSON string that will be
// stored, in declaration order (deterministic: the same graph projects the same bytes
// on every drive, which is what makes a re-projection on resume a no-op rather than a
// diff).
//
// 🔴 THE TWO DEPTH CHECKS -- WHICH AXIS EACH ONE CLOSES, AND AN HONEST NOTE ON WHAT
// EITHER IS WORTH TODAY.
//
// The pair is resolveExpansion's (fanout.go) and it is a pair for a reason 116 paid for:
//
//	checkValueDepth BEFORE the marshal -- the CRASH axis. json.Marshal RECURSES, and a
//	Go stack overflow is a `fatal error`, not a panic: unrecoverable, no deferred
//	recover() fires, the process dies. A check that runs after the encoder returns
//	cannot help, because on this axis the encoder never returns.
//
//	checkJSONDepth on the ENCODED BYTES before Set -- the WEDGE axis. WorkflowData.Set
//	is an unguarded bare map write, and every writer owes its own check against the
//	ceiling the reader will later enforce (json_depth.go), or the pair becomes WRITE
//	ACCEPTS / READ REFUSES, permanently.
//
// THE FIRST DRAFT OF THIS FUNCTION HAD ONLY THE SECOND, and 116's AF2 census
// (value_depth_census_test.go) caught it -- not review, not the author. Its message is
// the argument: "A guard that runs AFTER the call cannot help — that is what
// checkJSONDepth does and it is why it does not count here." That substitution, a
// checkJSONDepth standing in for a crash-axis guard, is the exact defect 116 spent a
// phase undoing at seven sites. Recorded rather than quietly fixed, because the near
// miss is the point: a phase closing that class had authored an eighth instance of it.
//
// NEITHER CHECK CAN FIRE AT THIS ENVELOPE'S SHAPE, and saying otherwise would be the
// vacuity this milestone keeps finding. boundaryDecl is three strings, so the value is
// two levels deep and carries no interface field a host value could enter through; and
// jsonNestingDepth deliberately skips string contents, so the encoded depth is a
// CONSTANT 3 no matter what a consumer names its nodes. There is no consumer input that
// reaches either axis -- not "the axis is covered", but "at this shape the axis has no
// input at all". A green from these two lines is evidence of NOTHING about consumer data.
//
// They are kept because they are regression armour for the shape change, not for today:
// the moment the envelope carries anything consumer-supplied and nested (an M24 policy
// blob, a per-door key set), both guards are already at the write, in the right order.
// The alternative -- omit them now and remember later -- is how a package acquires an
// unguarded writer. Their bite is a MUTATION bite for the same reason
// (boundary_envelope_test.go): no legal input can red them.
func encodeBoundaryEnvelope(decls []boundaryDecl) (string, error) {
	env := boundaryEnvelope{
		Version:    boundaryEnvelopeVersion,
		Boundaries: make([]boundaryEnvelopeEntry, len(decls)),
	}
	for i, d := range decls {
		env.Boundaries[i] = boundaryEnvelopeEntry{Doer: d.doer, Verifier: d.verifier, Sink: d.sink}
	}
	if verr := checkValueDepth(env, "boundary projection"); verr != nil {
		return "", verr
	}
	enc, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("%w: boundary projection is not JSON-encodable: %w", ErrValidation, err)
	}
	if derr := checkJSONDepth(enc, "boundary projection"); derr != nil {
		return "", derr
	}
	return string(enc), nil
}

// decodeBoundaryEnvelope reads a projection back, STRICTLY. Modelled on
// resolveExpansion (fanout.go): the stored form must be a string, it must parse, and
// its version must be one this build knows -- an unknown version is refused with a
// typed error rather than partially interpreted.
//
// Refusing UPWARD is the point. A future build's envelope decoded leniently by this one
// would silently drop whatever the new version added, and a projection that quietly
// loses a field is worse than one that refuses to be read.
//
// NOTHING IN THE ENGINE CALLS THIS (DEC-M23-SEAM-INERT). It is the compatibility
// contract, exercised by tests. If a future phase finds itself calling this to DECIDE
// something, that is the M24 policy question and it needs a ruling first.
func decodeBoundaryEnvelope(raw any) (boundaryEnvelope, error) {
	s, isStr := raw.(string)
	if !isStr {
		return boundaryEnvelope{}, fmt.Errorf("%w: boundary projection is not a string (got %T)", ErrValidation, raw)
	}
	var env boundaryEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return boundaryEnvelope{}, fmt.Errorf("%w: boundary projection malformed: %w", ErrValidation, err)
	}
	if env.Version != boundaryEnvelopeVersion {
		return boundaryEnvelope{}, fmt.Errorf("%w: %w: projection is version %d, this build reads version %d",
			ErrValidation, errBoundaryEnvelopeVersion, env.Version, boundaryEnvelopeVersion)
	}
	return env, nil
}

// projectBoundaries writes the projection. CALLED ONLY UNDER d.hasBoundaries -- the
// gate lives at the call site in Execute, not in here, so that a workflow declaring no
// boundary does not even make the call: no encode, no Set, no allocation on the drive
// path. That is the hasFanOut moat verbatim (dag.go), and the zero-determinism-tax
// claim is this project's headline rather than a nicety.
func (d *DAG) projectBoundaries(data *WorkflowData) error {
	enc, err := encodeBoundaryEnvelope(d.boundaries)
	if err != nil {
		return err
	}
	data.setReserved(boundariesKey, enc)
	return nil
}
