------------------------- MODULE MCExecutor -------------------------
(* Model-checking harness for Executor: supplies the concrete DAG          *)
(* (Nodes + Deps, which use tuple syntax that TLC .cfg files cannot         *)
(* express) and forwards the remaining constants (ContinueOnError,          *)
(* FailSet, MaxConc) from the .cfg by name.                                 *)
(*                                                                          *)
(* Concrete instance: diamond  n1 -> {n2, n3} -> n4.                        *)
EXTENDS FiniteSets, Naturals

CONSTANTS n1, n2, n3, n4, ContinueOnError, FailSet, MaxConc

MCNodes == {n1, n2, n3, n4}
MCDeps  == { <<n1,n2>>, <<n1,n3>>, <<n2,n4>>, <<n3,n4>> }

VARIABLES status, halted

INSTANCE Executor WITH Nodes <- MCNodes, Deps <- MCDeps
=============================================================================
