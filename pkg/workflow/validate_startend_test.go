package workflow

// TestValidate_StartAndEndNodes was DELETED by M23 phase 117 (T1c), together with the
// DAG.StartNodes and DAG.EndNodes fields it pinned.
//
// It was a mutation-kill test, and a good one: it existed because the gremlins mutants
// on the start/end identification loop survived, so it asserted the exact CONTENTS of
// both sets. Its stated justification was "these are public surface, so their
// correctness is a real contract" — and that justification is precisely what T1c
// removed. The fields were written three times each inside Validate and read NOWHERE in
// the engine; the loop that filled them ran on every Execute, because Validate is on the
// Execute path. Deleting the fields deletes the computation, and with it both the
// mutants and the only test that could kill them.
//
// Nothing was lost that still had a subject: the test asserted a property OF the deleted
// state, not of the graph. The dependency-free and dependent-free node sets are still
// computed where they are actually used — GetLevels derives its level-0 set from
// len(node.dependsOn) == 0 on every call, and that IS exercised by the executor on every
// run.
//
// This file is kept as the record. Delete it once M23 has shipped.
