package workflow

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 🔴 IF YOU ARE REFACTORING THIS FILE, READ THIS BLOCK AND NOTHING ELSE FIRST.
//
// What follows below is eight commits of accumulated argument, and it is worth reading --
// but a file that argues well is a file a reader trusts BY READING, and reading is exactly
// what cannot verify a guard. FOUR MECHANICAL PROPERTIES ARE WHAT KEEP THIS HONEST. Each
// was learned from a defect that had already shipped; none of them is prose:
//
//  1. THE ANTI-VACUITY ASSERTIONS LIVE IN THE SHARED SWEEP HELPER, NOT IN AN ARM. Every arm
//     calls claimScopeSweepPackage, so no arm can outlive the assertions that make it
//     meaningful. Move them into one arm and the others go silently vacuous -- which is
//     what 118-D1 was: the ban arm passed with the subject test forced always-false, alone,
//     while every other arm failed.
//
//  2. FORCING claimScopeQualify TO RETURN OUT-OF-SCOPE MUST RED ALL FIVE ARMS, EACH RUN
//     ALONE. This is the stop-condition, not a nice-to-have. It has been re-run after every
//     edit to that function -- five so far -- because a change there can silently DECOUPLE
//     the ban arm and reproduce 118-D1 with nobody looking for it. Run the arms alone: the
//     isolation is what made the original vacuity visible.
//
//  3. THE CORPUS IS RUN THROUGH THE SAME CODE THE BAN RUNS, WITH EACH FIXTURE'S OWN
//     QUALIFIER. A ratchet that re-implemented the check would prove only that the
//     re-implementation still works, and a fixture list nothing executes is decoration.
//
//  4. THE INNOCENT CORPUS IS ASSERTED NOT-RED, AND ITS COUNTER-WITNESS IS ASSERTED PRESENT
//     BY NAME. A ratchet is one-sided pressure: every future miss argues for a wider rule,
//     and an always-true rule satisfies every known-bad fixture at once. The innocent side
//     is the only thing that notices. See the HOLE-B paragraph for why deleting one
//     particular fixture would remove the evidence that a trade was ever made.
//
// THE PROSE BELOW CAN BE REFLOWED, RE-ORDERED OR CUT FREELY. THESE FOUR CANNOT MOVE WITHOUT
// A RE-BITE -- mutate the thing they assert, confirm the mutation is REACHED, and read the
// failure MESSAGE rather than the FAIL. A green after a change proves nothing about whether
// the change did anything.

// THE SCOPE-PHRASING GUARD — and WHAT IT DOES NOT CLOSE, which is the part that must be
// read before the green is.
//
// WHY IT EXISTS. M23's defining defect class (116-AF9) is A LOCALLY-TRUE STATEMENT
// RESTATED AT A WIDER SCOPE. It has eight instances across four authors in this
// milestone, and the fix for #1-4 authored #5 -- in the only copy that ships, at the site
// the phase was about. Two things have ever caught an instance: an independent reader,
// and a repo-wide phrase grep. Neither scales, and neither re-runs. This does.
//
// THE PROPERTY IT GUARDS (DEC-M23-NAMING). A declared boundary (D, V, S) asserts
// PRECEDENCE: on every route through the built graph, S does not occur before V. That is
// V dom S over control flow and NOTHING MORE. It is NOT "V verifies D's effect" and NOT
// "no doer's effect reaches S unverified": those are true at the control-flow scope and
// FALSE at the effect scope, and V may legitimately run BEFORE D. The wider sentence is
// the ninth instance waiting to be written.
//
// claimscope:prohibition — this block quotes the banned phrasings in order to DEFINE
// them. It is the first and most obviously legitimate use of the opt-out.
//
// 🔴 THE INSTRUMENT'S BOUND, STATED SO A GREEN CANNOT BE MISREAD AS CLOSURE.
//
// The population is complete and the detector is not. Those are different claims and
// only the first is proven:
//
//   - POPULATION: mechanically enumerated, and this half IS proven. go/parser in
//     ParseComments mode puts EVERY comment in ast.File.Comments -- including comments
//     inside struct literals and inside //go: directive blocks -- and the sweep parses
//     FILES rather than packages, so a build-tagged file is in the population too (see
//     parsePackageDir). claimScopeSweep asserts the parsed count equals the .go files on
//     disk, that every file carrying comment lines on disk yielded comments to the parser,
//     that boundary.go is among them, and that the trigger matched a non-zero number of
//     blocks and NOT all of them -- because an instrument that silently matched nothing,
//     and one that indiscriminately matched everything, both report the same green as one
//     that checked exactly what it claims.
//
//   - DETECTOR: A PATTERN LIST, AND A PATTERN LIST CAN ONLY CONTAIN PHRASINGS SOMEONE HAS
//     ALREADY BEEN BURNED BY. It cannot contain its author's blind spot. Every term in
//     bannedEffectScope below was learned from an instance that had already shipped. A
//     restatement phrased in vocabulary nobody has met yet passes this test. WIDENING THE
//     TRIGGER MOVES THAT HOLE, IT DOES NOT CLOSE IT -- the 118-D2 repair below is a wider
//     net, not a complete one. WHAT THE RATCHET BUYS IS NARROWER AND IS THE ONLY THING
//     CLAIMED FOR IT: a hole, ONCE FOUND, cannot silently reopen.
//
//     THAT IS NOT A HEDGE, AND HERE IS THE RECEIPT: the 118-D2 widening was bypassed by
//     118-D6 in the very next review round, using this package's own bare-letter idiom,
//     and all nine arms passed. The second widening closed that form and is not claimed to
//     close the next. Two forms are known to remain open even now, stated rather than
//     discovered later, and BOTH are open for reasons that must be read before anyone
//     closes them. Neither is a to-do:
//
//     🔴 HOLE-A WAS NOT ONE HOLE, IT WAS THREE (118-D9). Prose escapes the subject test
//     three ways; each needs a DIFFERENT relaxation; every relaxation was priced against
//     the real population -- 7,286 comment blocks, NOT the fixtures, which were green for
//     all three candidates and would have reported every one of them as free. ONE IS NOW
//     CLOSED AND TWO ARE DELIBERATELY LEFT OPEN AT A RECORDED PRICE:
//
//       1. CLOSED, AT ZERO MEASURED COST -- "doer" WITH NO SUBJECT TERM. The relaxation is
//          on "doer" ALONE and lands in BY-LETTERS; see claimScopeQualify, where both
//          halves are argued. Price paid: 1 new comment block and 9 new literals, ZERO
//          reds. The one new block is boundary.go's predicate-clause derivation, squarely
//          about a declared boundary, so the trigger is aimed where it claims; and 6 of the
//          9 literals are single tokens (a struct tag, this file's own matcher terms)
//          rather than prose. Fixture: d9-hole-a-doer-with-no-subject-term.txt.
//
//          WHY "doer" AND NOT THE OTHER TWO ROLE WORDS, so nobody re-tries it: relaxing on
//          all three costs 2 legitimate comments, BOTH FROM "sink" IN ITS ORDINARY SENSE,
//          both in signal_adversarial_concurrency_test.go -- the countingSink helper, and
//          an oracle describing an observable side effect. "sink" is standard taint and
//          data-flow vocabulary AND THIS PROJECT'S OWN TOOLING SPEAKS IT (`analyze sinks`,
//          "taint sink inventory", source-to-sink), so every comment in that register would
//          newly qualify. "verifier" carries crypto and qa senses. "doer" is a term of art
//          here with effectively no ordinary use, which is the entire reason it is safe.
//
//          🔴 AND THIS PRICE IS DATED BY CONSTRUCTION. "Zero cost" is a measurement of
//          today's tree, which makes it a LOWER BOUND and not a guarantee: the day "doer"
//          acquires an ordinary sense in this repo, form 1's relaxation acquires a cost
//          that this measurement could not have seen. No innocent fixture pins that,
//          deliberately -- a contrived "doer"-in-ordinary-sense block would assert a
//          population this codebase does not have, and a fixture a reader can dismiss as
//          unrealistic takes the credibility of the real ones with it. The same caveat
//          applies to every number in this section.
//       2. OPEN, PRICED -- ONE BARE ROLE LETTER and no other role token. Accepting a single
//          letter catches it, costs 1 comment plus 1 literal, and takes the matched
//          population from 35 blocks to 101. A tripling, for one form.
//       3. OPEN, PRICED, AND DECLINED FOR A REASON THAT IS NOT THE PRICE -- TWO BARE LETTERS
//          THAT ARE NOT V AND S, a D-V or D-S pair. Costs ZERO comments and exactly ONE
//          literal: THIS FILE'S OWN BAN MESSAGE, which quotes the wrong phrasing in order to
//          forbid it and, being a string literal, cannot carry the comment opt-out.
//
//          IT WOULD BE FREE IF THAT MESSAGE WERE REWORDED, AND IT WAS RULED THAT IT MUST NOT
//          BE. Rewording correct explanatory prose to accommodate a matcher inverts the
//          relationship: the guard would be shaping the writing it exists to police, and the
//          next author who meets a false positive would have a precedent for editing the
//          PROSE rather than pricing the RULE. That precedent costs more than the escape
//          class. The zero is also a SNAPSHOT -- from then on any two-letter co-occurrence
//          qualifies, and today's tree is a lower bound, not a guarantee.
//
//     So the subject test is not narrow through neglect. Each remaining widening reds prose
//     someone was right to write, and the price is recorded here so the next author argues
//     with the number rather than re-deriving it.
//
//     🔴 HOLE-B IS A PRICE RATHER THAN A GAP (118-D9). In a block that qualifies by
//     LETTERS alone, a restatement SPLIT ACROSS SENTENCES -- roles named in one sentence,
//     the widening asserted in the next -- is not seen, because the per-sentence rule on
//     claimScopeViolations examines only sentences that name a role.
//
//     DO NOT CLOSE IT. CLOSING IT IS A REGRESSION, AND THE COUNTER-WITNESS IS CHECKED IN:
//     testdata/claimscope/innocent/bare-letter-block-with-an-unrelated-sentence.txt. That
//     fixture is a real comment from boundary_action_kind.go, and it has HOLE-B'S EXACT
//     SHAPE -- a letters-qualified block whose second sentence carries a banned term in an
//     ordinary engineering sense and names no role. The two are structurally the same
//     block. Any rule that catches the one reds the other.
//
//     MEASURED, NOT ARGUED: removing the per-sentence rule -- by deleting it outright, and
//     independently by forcing every block to qualify as if by words -- reds the ban arm
//     AND reds that innocent fixture, and reds NO other innocent fixture. Both routes,
//     same single casualty. So HOLE-B is precisely the price that fixture was chosen to
//     pay, and a fixture asserting HOLE-B is caught would stand in direct contradiction
//     with the innocent corpus.
//
//     The failure this paragraph exists to prevent: someone reads "known to remain open",
//     closes it, reds a legitimate comment, and DELETES THE INNOCENT FIXTURE to make the
//     red go away -- which would remove the only witness that the trade was ever made.
//
// So this NARROWS the class; it does not CLOSE it. A green here means "no comment in this
// package restates the boundary property using a phrasing we have already been burned
// by." Reading it as "the class is closed" is itself the defect this file is about.
//
// It also does not cover: files outside pkg/workflow (docs/, examples/, .planning/), and
// prose in commit messages. Scope is deliberately small -- this guards ONE property's
// phrasing, at HEAD, in one package. The general form (every comment asserting an
// encoder/persist property must name a backend in scope) is milestone-level and is not
// this file.
//
// TWO DEFECTS FOUND IN THIS FILE BY AN INDEPENDENT READER, AND WHAT EACH FORCED. Both were
// found by attacking the guard with exactly what it asserts, and both are recorded here
// because the repair for each is load-bearing and would otherwise look like a preference:
//
//   - 118-D1, the ban arm could not detect its own vacuity. With the trigger forced
//     always-false and each arm run ALONE, the census arm FAILED, the literal arm FAILED,
//     and the ban arm PASSED. Its coupling to the census was prose and suite co-location,
//     never code: renaming or deleting the census would have left the ban silently vacuous
//     forever. The repair is that the anti-vacuity assertions now live INSIDE
//     claimScopeSweep, which every arm calls, so no arm can outlive them; and the ban arm
//     asserts it dispositioned THE WHOLE population the sweep reported rather than merely
//     that it saw something.
//
//   - 118-D2, the trigger could not see the sentence this guard exists to catch. The
//     mandatory "boundar" conjunct made the canonical bad sentence -- which names doer,
//     verifier and sink, and omits only the word "boundary" -- invisible to all three arms.
//     The diagnosis was already written one line above the gate that still had it. The
//     repair is the disjunction in aboutTheBoundary plus the ratchet corpus, which turns
//     that one found hole into a re-running assertion instead of a paragraph.
//
// WHY THE TRIGGER IS NOT INVERTED, since that is the obvious alternative and it was
// weighed and refused: triggering on the banned vocabulary ANYWHERE plus an opt-out mark
// would be more complete and would not survive. "effect", "verifies", "validated",
// "mediates" are ordinary words in a workflow engine; an inverted trigger reds on
// legitimate prose constantly, and a guard that cries wolf is gutted by the first person
// it inconveniences. A deleted guard protects nothing. Survival is the prior constraint
// over completeness, and this file states that as a choice rather than hiding it.

// claimScopeAllow marks a comment block that quotes the banned phrasing IN ORDER TO
// FORBID IT -- this file's own doc comment, and boundary.go's contract, both must say the
// wrong sentence out loud to rule it out.
//
// It is an OPT-OUT, so it is also the guard's softest edge: anyone can mark a block and
// then write a genuine restatement inside it. That is why every marked block is LOGGED by
// name and line on every run rather than silently skipped -- the escape hatch is visible
// in the test's own output, and a growing list of them is the signal that someone is
// routing around the check.
//
// IT IS ALSO THE ONLY OPT-OUT IN THIS FILE, DELIBERATELY. The ratchet corpus below created
// obvious pressure for a second one (a fixture collides with the ban by construction) and
// the answer was to move the corpus out of the package rather than to add a name-keyed
// exemption. Two opt-outs is how a guard becomes advisory.
const claimScopeAllow = "claimscope:prohibition"

// bannedEffectScope is the learned list. Each term names the effect scope -- something
// happening to D's WORK -- rather than the control-flow scope, which is the only scope
// M23 proves.
//
// 🔴 MATCHED ON WORD BOUNDARIES, AND THAT IS A DEFECT FIX, NOT A REFINEMENT (118-D12).
// These were compared as BARE SUBSTRINGS, so "effect" matched inside INEFFECTIVE,
// EFFECTIVE and EFFECTIVELY -- ordinary engineering words carrying no scope claim at all.
// engineer-118b hit it on the first sentence it wrote after the 118-D6 widening, while
// documenting 118-D10, NOT while probing the guard: an ordinary line about a post-Build
// mutation being silently ineffective red the ban arm. It reworded rather than marking the
// block, which was right, but rewording is the wrong permanent answer -- the next author
// will not know the history and will reach for the opt-out or for git rm. The sentence is
// checked in verbatim as an innocent fixture; it is not quoted here, because quoting it
// would drag THIS block into scope and red the definition below.
//
// THIS IS THE FAILURE MODE RULING 2 EXISTS TO PREVENT, ARRIVING IN THE TREE RATHER THAN IN
// A HYPOTHESIS. A guard that reds on correct writing is deleted by whoever it first
// inconveniences, and this one had already done it once, within hours, to an author writing
// a required doc line. The tree carries many more latent ones -- effectiveMaxDepth, a timer
// that "parks effectively forever", the effective_stack arithmetic in value_depth.go --
// invisible today only because their blocks do not qualify, and waiting for the next time
// the subject test widens.
//
// THE PLURAL AND PREFIXED FORMS ARE LISTED EXPLICITLY rather than matched by a cleverer
// pattern, because word-boundarying can only NARROW, and every form that would newly escape
// has to be a decision somebody made rather than a byproduct of a regex. Kept: effects,
// effected. Added: unmediated, which substring matching used to catch inside "mediated" and
// which the dec-m23 fixture depends on. Deliberately allowed to escape: effective,
// ineffective, effectively, remediated, remediation -- every one of them ordinary
// vocabulary, none of them a scope claim.
var bannedEffectScope = []string{
	"effect", "effects", "effected",
	"verifies", "verified", "unverified",
	"mediates", "mediated", "unmediated",
}

// bannedEffectScopeRE is bannedEffectScope compiled with word boundaries. Built once: the
// ban arm runs it over every sentence of every matched block on every run.
var bannedEffectScopeRE = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(bannedEffectScope))
	for _, term := range bannedEffectScope {
		out = append(out, regexp.MustCompile(`\b`+regexp.QuoteMeta(term)+`\b`))
	}
	return out
}()

// aboutTheBoundary reports whether a block of prose is ASSERTING SOMETHING ABOUT A
// DECLARED BOUNDARY, which is what makes its phrasing this guard's business.
//
// claimscope:prohibition — quotes the banned sentence as the example that shaped the
// trigger.
//
// Judged over the WHOLE BLOCK, never sentence by sentence, and that is a correction the
// prototype forced rather than a preference: the canonical bad sentence -- "no doer's
// effect reaches S unverified" -- contains neither "boundary" nor "verifier" nor "sink".
// A sentence-scoped trigger scored ZERO hits on a tree that contained that exact sentence.
//
// AND THAT DIAGNOSIS WAS WRITTEN WHILE THE CAUSE SURVIVED THE FIX. The prototype's repair
// widened the TEXT SEARCHED from sentence to block and kept a MANDATORY "boundar"
// conjunct, so the sentence quoted three lines up -- which names doer, verifier and sink
// and omits only "boundary" -- still scored zero. An independent reader planted exactly it
// and all three arms passed (118-D2). The subject test is now a DISJUNCTION over the role
// vocabulary: a block qualifies if it names the boundary, OR writes the (D, V, S) triple,
// OR pairs the doer with either of the other two roles. It stays ANDed with a role name so
// that "boundary" alone -- store boundaries, trust boundaries, save-at-boundaries, all
// common in this package -- does not drag unrelated prose in.
//
// Measured against the tree before it was adopted: 15 matched blocks became 20 and 7
// matched literals became 9, with ZERO of the newly-matched carrying banned vocabulary. A
// widening that reds innocent prose is a defect and not a success, so the count was taken
// first.
//
// 🔴 THEN IT WAS BYPASSED AGAIN, BY THE PACKAGE'S OWN IDIOM (118-D6). The reviewer planted
// in dag.go a comment saying that on every route through the built graph D's effect reaches
// S only after V has run, so no effect of D ever reaches S unverified. Two banned terms, a
// genuine widening, and ALL NINE ARMS PASSED -- because it names the roles BY THEIR
// LETTERS, and the word vocabulary above cannot see a letter. It avoided the parenthesised
// triple, which was already covered.
//
// The severity is not that a hole existed; this file declares that holes exist. It is that
// BARE-LETTER ROLE NAMING IS THIS PACKAGE'S DOMINANT IDIOM -- boundary.go writes "V dom S"
// and "S does not occur before V" -- so the likeliest next instance was phrased in exactly
// the form the guard could not see. Hence the second qualifying path: a bare capital V and
// a bare capital S, matched case-SENSITIVELY on the unlowered text, because lowercase v and
// s are two of the most common identifiers in Go.
//
// WHY V AND S RATHER THAN ANY TWO OF D, V, S, which was the first thing tried and is worse:
// "any two" matched 34 blocks and RED THREE LEGITIMATE SITES, one of them this guard's own
// ban message ("V verifies D's work"), which lives in a string literal and therefore cannot
// carry the comment opt-out at all. "All three letters" reds nothing but misses "V dom S",
// the very idiom that makes this MAJOR. V-with-S catches the planted sentence and both
// quoted idioms, and takes the matched population from 20 blocks to 30 and 9 literals to 15
// with ZERO reds. Every one of those numbers was measured on a worktree at a committed
// head, not reasoned about.
func aboutTheBoundary(block string) bool {
	return claimScopeQualify(block) != claimScopeOutOfScope
}

// claimScopeQualifier records HOW a block qualified, and the ban treats the two paths
// differently because they carry different evidence strength. See claimScopeViolations.
type claimScopeQualifier int

const (
	claimScopeOutOfScope claimScopeQualifier = iota
	// claimScopeByWords: named a role IN WORDS and named the boundary. Strong evidence
	// that the whole block is about a declared boundary.
	claimScopeByWords
	// claimScopeByLetters: named the roles only as bare capitals. Enough to put the block
	// in scope, not enough to assume every sentence in it is on the subject.
	claimScopeByLetters
)

// claimScopeBareV and claimScopeBareS match a role letter standing alone. Case-sensitive
// and anchored on word boundaries: "V" in "V dom S" matches, "V" in "VALUE" does not, and
// lowercase "v" -- a Go idiom in every range loop in this repo -- never does.
var (
	claimScopeBareV = regexp.MustCompile(`\bV\b`)
	claimScopeBareS = regexp.MustCompile(`\bS\b`)
	claimScopeBareD = regexp.MustCompile(`\bD\b`)
)

func claimScopeQualify(block string) claimScopeQualifier {
	l := strings.ToLower(block)
	named := func(s string) bool { return strings.Contains(l, s) }

	// A role name must appear. Without this the trigger fires on any prose that says
	// "boundary", and this package is full of unrelated ones.
	role := named("doer") || named("verifier") || named("sink") || named("(d, v, s)")
	subject := named("boundar") || named("(d, v, s)") || (named("doer") && (named("verifier") || named("sink")))
	if role && subject {
		return claimScopeByWords
	}

	// HOLE-A, FORM 1: "doer" with no subject term. Qualifies as BY-LETTERS, and the arm it
	// lands in is the whole decision -- 118-D9.
	//
	// Promoting it to BY-WORDS instead would not merely widen scope, it would move these
	// blocks into the STRICT arm: claimScopeViolations checks EVERY sentence of a by-words
	// block, so a forty-line comment that says the word once would have all forty lines
	// checked. By-letters is the honest classification anyway, and it is the one this file
	// already defines -- enough to put the block in scope, not enough to assume every
	// sentence in it is on the subject.
	//
	// AND IT RELAXES ON "doer" ALONE, WHICH IS NOT A STYLE CHOICE. The three role words
	// carry different risk in THIS repo: "doer" is a term of art here with no ordinary
	// sense; "verifier" has one (crypto verifiers, verifier binaries); and "sink" is
	// standard data-flow and streaming vocabulary that this project's own tooling speaks --
	// taint sinks, source-to-sink, sink inventories -- so relaxing on it would newly qualify
	// every comment written in that register. Measured cost of relaxing on all three: 2
	// legitimate comments, both from "sink" used ordinarily. Relaxing on "doer" alone closes
	// the form without buying that risk.
	if named("doer") {
		return claimScopeByLetters
	}
	if claimScopeBareV.MatchString(block) && claimScopeBareS.MatchString(block) {
		return claimScopeByLetters
	}
	return claimScopeOutOfScope
}

// claimScopeNamesARole reports whether a SINGLE sentence mentions a boundary role at all,
// in words or as a bare letter. It is the weakest possible relevance test and it is applied
// in only one place, for the reason given on claimScopeViolations.
func claimScopeNamesARole(sentence string) bool {
	l := strings.ToLower(sentence)
	if strings.Contains(l, "doer") || strings.Contains(l, "verifier") || strings.Contains(l, "sink") {
		return true
	}
	return claimScopeBareD.MatchString(sentence) ||
		claimScopeBareV.MatchString(sentence) ||
		claimScopeBareS.MatchString(sentence)
}

// claimScopeSentences splits prose on terminators. Sentence granularity is what makes the
// failure message point at the offending clause instead of at a 40-line comment.
func claimScopeSentences(text string) []string {
	flat := strings.NewReplacer("\n", " ", "\t", " ").Replace(text)
	var out []string
	var cur strings.Builder
	for _, r := range flat {
		cur.WriteRune(r)
		if r == '.' || r == ';' || r == '!' || r == '?' {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// claimScopeHit is one banned term found in one sentence.
type claimScopeHit struct {
	sentence string
	term     string
}

// claimScopeViolations is THE prohibition, extracted into one function so that the arm
// which runs it over the real tree and the arm which runs it over the synthetic corpus are
// running THE SAME CODE. A ratchet that re-implemented the check would prove only that the
// re-implementation still worked.
//
// The subject test is deliberately NOT applied here: callers decide what is in scope. That
// split is what lets the corpus assert the two halves separately and report which one went
// blind.
//
// 🔴 THE QUALIFIER IS TAKEN AS AN ARGUMENT BECAUSE THE 118-D6 WIDENING WOULD OTHERWISE HAVE
// RED LEGITIMATE PROSE, AND THAT IS THE FAILURE RULING 2 EXISTS TO PREVENT. Judging the
// SCOPE over the whole block and the PROHIBITION sentence by sentence is right for the word
// vocabulary -- a block that says "boundary" and "verifier" is about the boundary
// throughout. It is NOT right for bare letters. boundary_action_kind.go carries a block
// whose first sentence names the roles by letter, and whose LATER sentence reports that a
// compile-time assertion was checked by machine -- ordinary engineering prose that names no
// role at all. Block-scoped banning reds that second sentence, in another author's
// committed file, for writing correctly. The block is checked in verbatim as an innocent
// fixture (testdata/claimscope/innocent/bare-letter-block-with-an-unrelated-sentence.txt),
// which is where the exact wording belongs: quoting it HERE is what the corpus exists to
// avoid, and this comment reds itself if it tries.
//
// So for a block that qualified ONLY by letters, a sentence is examined only if that
// sentence itself names a role. Blocks that qualified by WORDS are unchanged, which is why
// this cannot regress 118-D2: the sentence-scoped trigger that scored zero back then was
// the SCOPE test, and the scope test is still judged over the whole block.
//
// ITS BOUND, AND IT IS A REAL ONE: in a letters-only block, a restatement SPLIT ACROSS
// SENTENCES -- roles named in one, the widening asserted in the next -- is not seen. That
// hole is bought deliberately, because the alternative was reding correct prose, and a
// guard that reds on correct prose gets deleted. If anyone ever finds such a sentence, it
// becomes a fixture and cannot reopen.
func claimScopeViolations(text string, qualifier claimScopeQualifier) []claimScopeHit {
	var out []claimScopeHit
	for _, s := range claimScopeSentences(text) {
		if qualifier == claimScopeByLetters && !claimScopeNamesARole(s) {
			continue
		}
		low := strings.ToLower(s)
		for i, re := range bannedEffectScopeRE {
			if re.MatchString(low) {
				out = append(out, claimScopeHit{sentence: strings.TrimSpace(s), term: bannedEffectScope[i]})
			}
		}
	}
	return out
}

// parsePackageDir parses every .go file in this package directory, test files included --
// a restatement in a test's assertion message is as durable as one in shipped source, and
// three of this milestone's instances lived in test prose.
//
// FILE-BASED, NOT PACKAGE-BASED, and that is the difference between seeing this package
// and seeing most of it. parser.ParseDir groups files into PACKAGES, and staticcheck
// deprecated it (SA1019) for precisely the reason that matters here: it "does not consider
// build tags when associating files with packages". A BUILD-TAGGED file is exactly the
// kind this repo has already been bitten by twice -- the 116-AF11 adversarial file that
// nothing ran, and this phase's own boundary_rollback_arm_test.go, which `_arm` silently
// excluded as a GOARCH suffix. Reading the directory and parsing each file INDIVIDUALLY
// makes tags irrelevant: a file on disk is in the population whether or not any build
// configuration would compile it.
//
// It also returns, per file, the number of lines that OPEN a comment on disk, counted by a
// raw byte scan that shares no code with go/parser. That is the independent term of the
// coverage oracle in claimScopeSweep: "I parsed every file" and "I saw the comments in the
// files I parsed" are two claims, and a count of parsed files can only ever check the
// first.
func parsePackageDir(t *testing.T) (*token.FileSet, map[string]*ast.File, int, map[string]int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "the sweep must read this package's directory; a read error is a BLIND sweep")

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	rawCommentLines := map[string]int{}
	onDisk := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		onDisk++
		f, perr := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		require.NoError(t, perr, "the sweep must parse %s; a parse error is a BLIND sweep, not a passing one", e.Name())
		files[e.Name()] = f

		src, rerr := os.ReadFile(e.Name())
		require.NoError(t, rerr, "the sweep must read %s to check its own coverage", e.Name())
		sc := bufio.NewScanner(bytes.NewReader(src))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") {
				rawCommentLines[e.Name()]++
			}
		}
		require.NoError(t, sc.Err(), "the raw scan of %s must complete; a truncated scan is a weaker oracle silently", e.Name())
	}
	return fset, files, onDisk, rawCommentLines
}

// claimScopeMatch is one comment block or string literal the subject test fired on. It
// carries HOW it qualified, because the ban treats the two paths differently and a match
// that lost its qualifier would silently be banned under the stricter rule.
type claimScopeMatch struct {
	pos       token.Position
	text      string
	allowed   bool
	qualifier claimScopeQualifier
}

// claimScopeSweep is the population, and every arm in this file is built on it.
//
// 🔴 THE ANTI-VACUITY ASSERTIONS LIVE HERE, IN THE SHARED HELPER, AND THAT PLACEMENT IS
// THE WHOLE 118-D1 REPAIR. They used to live in one arm, with the other arms depending on
// it through prose and suite co-location. Measured: with the subject test forced
// always-false and each arm run alone, the census FAILED, the literal arm FAILED, and the
// ban arm PASSED VACUOUSLY -- so deleting or renaming the census would have disarmed the
// ban with nothing turning red. Assertions in a helper cannot be orphaned that way: an arm
// that stops calling the helper stops having a population at all.
type claimScopeSweep struct {
	fset *token.FileSet
	// parsed is kept so an arm can re-derive the counts independently and check this
	// struct's own arithmetic, rather than trusting the number it was handed.
	parsed   map[string]*ast.File
	onDisk   int
	comments int
	blocks   []claimScopeMatch
	literals int
	litHits  []claimScopeMatch
}

func claimScopeSweepPackage(t *testing.T) claimScopeSweep {
	t.Helper()
	fset, files, onDisk, rawCommentLines := parsePackageDir(t)

	sw := claimScopeSweep{fset: fset, parsed: files, onDisk: onDisk}
	sawBoundaryGo, sawCanonical := false, false
	var blind []string
	for name, f := range files {
		if name == "boundary.go" {
			sawBoundaryGo = true
		}
		if rawCommentLines[name] > 0 && len(f.Comments) == 0 {
			blind = append(blind, name)
		}
		for _, cg := range f.Comments {
			sw.comments++
			text := cg.Text()
			q := claimScopeQualify(text)
			if q == claimScopeOutOfScope {
				continue
			}
			sw.blocks = append(sw.blocks, claimScopeMatch{
				pos:       fset.Position(cg.Pos()),
				text:      text,
				allowed:   strings.Contains(text, claimScopeAllow),
				qualifier: q,
			})
			if strings.Contains(text, "Precedence(V, S)") {
				sawCanonical = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sw.literals++
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				v = lit.Value // a raw literal that will not unquote is still scannable text
			}
			if q := claimScopeQualify(v); q != claimScopeOutOfScope {
				sw.litHits = append(sw.litHits, claimScopeMatch{pos: fset.Position(lit.Pos()), text: v, qualifier: q})
			}
			return true
		})
	}
	sort.Strings(blind)

	require.True(t, sawBoundaryGo, "the sweep did not even see boundary.go — it is aimed at the wrong target")
	require.Equal(t, onDisk, len(files),
		"THE POPULATION CLAIM: every .go file on disk in this directory must have been parsed. A gap here "+
			"means the sweep is reporting on a subset while reading as if it covered the package")
	require.Empty(t, blind,
		"THE COVERAGE CLAIM: these files carry comment lines on disk and yielded ZERO comments to the parser, "+
			"so the sweep is reading them and seeing nothing: %v. (The oracle is one-directional on purpose: "+
			"the parser legitimately sees MORE than a line-prefix scan, because a trailing comment does not open "+
			"its line. Only the blind direction is a defect.)", blind)
	require.NotEmpty(t, sw.blocks,
		"ZERO comment blocks matched the subject test — every arm built on this sweep is vacuous. This is the "+
			"failure that a renamed file, a moved package or a vocabulary drift produces, and it reports the "+
			"same green as a real pass unless it is asserted here")
	require.Less(t, len(sw.blocks), sw.comments,
		"the subject test matched EVERY one of the %d comment blocks in this package. A subject test that is "+
			"always true passes every fixture in the corpus while proving nothing, and that is the exact shape "+
			"a future widening will be tempted into", sw.comments)
	require.NotEmpty(t, sw.litHits, "no string literal matched — the literal arm would pass vacuously")
	require.True(t, sawCanonical,
		"no comment states the property in the canonical Precedence(V, S) form. The ban arm is trivially "+
			"satisfiable by DELETING the statement, so its presence is asserted here rather than assumed")

	return sw
}

// claimScopeFixture is one known-bad (or known-innocent) block of prose from the corpus.
type claimScopeFixture struct {
	name string
	text string
	// author is a SELF-REPORTED token naming who produced this block. Structured rather
	// than free text for one reason: the signal worth watching is the RATIO, and a corpus
	// drifting toward self-authored witnesses is the failure this field exists to make
	// visible. Free text cannot be counted.
	author string
	notes  []string
}

// claimScopeFixtureDir holds the ratchet corpus.
//
// 🔴 IT IS OUTSIDE THE PACKAGE SOURCE BY CONSTRUCTION, NOT BY TASTE. A fixture IS a block
// of prose that fires the subject test AND carries forbidden vocabulary -- that is what
// makes it a fixture. As a Go string literal it would red the literal arm; as a comment it
// would red the ban arm. The alternative was a second, name-keyed opt-out, and that was
// refused: this guard already has one escape hatch and two is how a guard becomes
// advisory. Go tooling ignores testdata/ and parsePackageDir skips directories, so the
// corpus is checked in, reviewable and version-controlled while being invisible to the
// sweep that would otherwise red on it.
//
// Fixture text is stored as the parser hands prose to the check -- comment markers already
// stripped -- so the corpus and the tree feed the check in the same shape.
const claimScopeFixtureDir = "testdata/claimscope"

// claimScopeInnocentDir holds the corpus of prose that must NOT be flagged.
//
// It exists because a ratchet is one-sided pressure: every future miss argues for a wider
// subject test, an always-true subject test satisfies every known-bad fixture, and nothing
// on the known-bad side can tell the difference. These are the control.
const claimScopeInnocentDir = claimScopeFixtureDir + "/innocent"

func claimScopeLoadFixtures(t *testing.T, dir string) []claimScopeFixture {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "the corpus at %s must be readable; a missing corpus is a ratchet that never fired", dir)

	var out []claimScopeFixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, rerr, "fixture %s must be readable", e.Name())

		// A '#' line is METADATA and is stripped before the text ever reaches the check. It
		// has to be stripped rather than tolerated: the file's content IS the block the
		// check runs on, so a metadata line left in would silently become part of the
		// witness and change what the fixture proves.
		var prose, notes []string
		author := ""
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "#") {
				prose = append(prose, line)
				continue
			}
			meta := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			if rest, ok := strings.CutPrefix(meta, "author:"); ok {
				author = strings.TrimSpace(rest)
				continue
			}
			notes = append(notes, meta)
		}
		name := strings.TrimSuffix(e.Name(), ".txt")
		require.NotEmpty(t, author,
			"fixture %s declares no '# author: <token>' line. Every fixture must DISCLOSE who produced it, "+
				"because a fixture written by the author of the rule is a SELF-TEST -- it cannot fail for the "+
				"reason an independently-found one can, since the mind that wrote the rule wrote the case. "+
				"NOTE WHAT THIS ARM DOES AND DOES NOT DO: it enforces DISCLOSURE. It cannot verify the token, "+
				"which is self-reported. A green means every fixture names an author, NEVER that any of them "+
				"was independently authored", name)
		out = append(out, claimScopeFixture{
			name:   name,
			text:   strings.Join(prose, "\n"),
			author: author,
			notes:  notes,
		})
	}
	require.NotEmpty(t, out, "the corpus at %s is EMPTY, so the arm below asserts nothing", dir)

	// THE TALLY, LOGGED RATHER THAN ASSERTED, and both halves of that are deliberate.
	//
	// Logged, because the ratio is the signal: a corpus drifting toward witnesses written
	// by whoever wrote the rule loses the property that makes a ratchet worth having, and
	// that drift is invisible one fixture at a time. This follows the claimScopeAllow
	// precedent already in this file -- the soft thing is printed on every run so a reader
	// counting them can watch it grow.
	//
	// NOT asserted, because the token is SELF-REPORTED. A threshold on it would be a
	// threshold on what fixtures CLAIM about themselves, and asserting a floor on
	// unverifiable self-reports is how a number starts reading as a guarantee.
	byAuthor := map[string]int{}
	for _, fx := range out {
		byAuthor[fx.author]++
	}
	authors := make([]string, 0, len(byAuthor))
	for a := range byAuthor {
		authors = append(authors, a)
	}
	sort.Strings(authors)
	for _, a := range authors {
		t.Logf("corpus %s: %d fixture(s) declare author %q (self-reported, unverifiable)", dir, byAuthor[a], a)
	}
	return out
}

// TestBoundaryClaimScope_SweepSeesThePopulation is the population REPORT, and it is no
// longer where the anti-vacuity assertions live -- those moved into claimScopeSweepPackage
// so that no arm can outlive them (118-D1). What is left here is a real job and not a
// formality: it re-derives every count from the parsed files INDEPENDENTLY of the
// bookkeeping the sweep did while walking them, so an accounting bug in the struct that
// every other arm now trusts fails HERE, by name.
func TestBoundaryClaimScope_SweepSeesThePopulation(t *testing.T) {
	sw := claimScopeSweepPackage(t)

	comments, blocks, literals, litHits := 0, 0, 0, 0
	for _, f := range sw.parsed {
		for _, cg := range f.Comments {
			comments++
			if aboutTheBoundary(cg.Text()) {
				blocks++
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			literals++
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				v = lit.Value
			}
			if aboutTheBoundary(v) {
				litHits++
			}
			return true
		})
	}

	require.Equal(t, comments, sw.comments, "the sweep's comment count disagrees with a re-count of the same files")
	require.Equal(t, blocks, len(sw.blocks), "the sweep's matched-block SET is not the set a re-count produces")
	require.Equal(t, literals, sw.literals, "the sweep's literal count disagrees with a re-count of the same files")
	require.Equal(t, litHits, len(sw.litHits), "the sweep's matched-literal SET is not the set a re-count produces")

	for _, b := range sw.blocks {
		if strings.Contains(b.text, "Precedence(V, S)") {
			t.Logf("canonical statement found at %s", b.pos)
		}
	}
	t.Logf("population: %d files, %d comment blocks, %d of them in scope; %d string literals, %d in scope",
		len(sw.parsed), sw.comments, len(sw.blocks), sw.literals, len(sw.litHits))
}

// TestBoundaryClaimScope_NoEffectScopeRestatement is the ban itself.
//
// It asserts that it DISPOSITIONED THE WHOLE POPULATION the sweep reported -- every matched
// block either scanned or logged as opted-out, with the two adding back up. "The arm saw
// something" was the shape that let it pass vacuously in the first place (118-D1); a count
// that must reconcile is the shape that cannot.
func TestBoundaryClaimScope_NoEffectScopeRestatement(t *testing.T) {
	sw := claimScopeSweepPackage(t)

	allowed, scanned := 0, 0
	for _, b := range sw.blocks {
		if b.allowed {
			allowed++
			// Logged, never silent: the opt-out is the guard's softest edge and it
			// belongs in the output where a reviewer counting them can see it grow.
			t.Logf("ALLOWED (marked %s): %s", claimScopeAllow, b.pos)
			continue
		}
		scanned++
		for _, v := range claimScopeViolations(b.text, b.qualifier) {
			require.Failf(t, "scope restatement in a comment",
				"%s: this sentence asserts something about a declared boundary using EFFECT-SCOPE "+
					"vocabulary (%q). M23 proves PRECEDENCE only — S never occurs before V, over "+
					"control flow. It does NOT prove that V verifies D's work: V may legitimately "+
					"run before D. Restating it that way is 116-AF9, a locally-true statement widened, "+
					"and it would be this milestone's ninth instance.\n\nOffending sentence:\n  %s\n\n"+
					"If this comment quotes the wrong phrasing in order to FORBID it, mark the comment "+
					"block with %s.",
				b.pos, v.term, v.sentence, claimScopeAllow)
		}
	}

	require.Equal(t, len(sw.blocks), allowed+scanned,
		"the ban did not disposition the whole population: the sweep matched %d blocks and this arm accounted "+
			"for %d. A block that is neither scanned nor logged as opted-out is one this guard silently skipped",
		len(sw.blocks), allowed+scanned)
	require.NotZero(t, scanned,
		"every one of the %d matched blocks carries the %s opt-out, so this arm scanned nothing. That is the "+
			"vacuous pass an opt-out makes possible, and it is asserted rather than assumed",
		len(sw.blocks), claimScopeAllow)
	t.Logf("ban: %d block(s) scanned, %d carrying the %s opt-out", scanned, allowed, claimScopeAllow)
}

// TestBoundaryClaimScope_NoEffectScopeInUserFacingStrings extends the sweep past comments
// to STRING LITERALS.
//
// DECLARED AS AN EXTENSION, NOT SLIPPED IN: the task specified comments. Literals are
// included because an error message or an assertion message restating the property at the
// wider scope is strictly WORSE than a comment doing so -- a comment is read by
// maintainers, an error string is read by consumers. Measured before it was written: the
// literals in this package that are in scope carry no forbidden vocabulary, so this arm
// costs nothing today and is armour for tomorrow.
//
// Its own blind cell, stated: a literal built by concatenation or fmt at run time is seen
// only in the fragments the source contains.
func TestBoundaryClaimScope_NoEffectScopeInUserFacingStrings(t *testing.T) {
	sw := claimScopeSweepPackage(t)

	for _, l := range sw.litHits {
		for _, v := range claimScopeViolations(l.text, l.qualifier) {
			require.Failf(t, "scope restatement in a string literal",
				"%s: this string literal asserts something about a declared boundary using the wider "+
					"vocabulary (%q). The property is PRECEDENCE over control flow, and a consumer reads "+
					"this text.\n\nOffending literal:\n  %s",
				l.pos, v.term, l.text)
		}
	}
	t.Logf("%d string literal(s) in scope scanned", len(sw.litHits))
}

// TestBoundaryClaimScope_RatchetCatchesKnownBadPhrasings runs the check over the SYNTHETIC
// corpus, and it is the arm that makes a found hole permanent.
//
// 🔴 WHY A LIST OF KNOWN-BAD SENTENCES IS NOT ENOUGH ON ITS OWN. The ban arm scans real
// comments in this tree. A corpus sentence is not in the tree -- it cannot be, or the ban
// would red on it -- so nothing in the ban ever touches it and the list quietly becomes
// decoration. Each fixture is therefore run as an ORACLE OVER SYNTHETIC INPUT: the check
// itself is invoked on the fixture text and asserted to CATCH it. That is the same
// distinction that made 118-D1 a finding -- "the arm saw something" against "the arm saw
// the population it claims".
//
// The two halves are asserted separately because they fail for different reasons and the
// message has to say which: a fixture the subject test misses means the trigger narrowed
// (118-D2's exact shape), a fixture the prohibition misses means a term left the list.
func TestBoundaryClaimScope_RatchetCatchesKnownBadPhrasings(t *testing.T) {
	fixtures := claimScopeLoadFixtures(t, claimScopeFixtureDir)

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			require.NotContains(t, fx.text, claimScopeAllow,
				"fixture %q carries the opt-out marker, so it would be SKIPPED by the ban rather than caught. "+
					"A fixture that opts itself out proves nothing", fx.name)
			q := claimScopeQualify(fx.text)
			require.NotEqual(t, claimScopeOutOfScope, q,
				"THE SUBJECT TEST WENT BLIND to a known-bad block. This block was caught once; it is in the "+
					"corpus because someone had to find it the expensive way, and claimScopeQualify no longer "+
					"fires on it. That is 118-D2 and 118-D6 reopening.\n\nFixture %s:\n  %s",
				fx.name, strings.TrimSpace(fx.text))
			// The fixture's OWN qualifier is passed, so this arm runs the identical path the
			// ban would run on it -- including the stricter per-sentence rule that applies to
			// letters-only blocks. Hard-coding the permissive qualifier here would prove the
			// fixture is caught by a check the tree never performs.
			hits := claimScopeViolations(fx.text, q)
			require.NotEmpty(t, hits,
				"THE PROHIBITION WENT BLIND to a known-bad block: the subject test still fires on it, but no "+
					"term in bannedEffectScope matches any sentence the ban would examine. A term was removed, "+
					"the sentence splitter changed, or the letters-only sentence rule now skips it."+
					"\n\nFixture %s:\n  %s",
				fx.name, strings.TrimSpace(fx.text))
			// Logged on every run, not merely recorded on disk, so a reader of the OUTPUT
			// can see what each fixture CLAIMS about its own origin without opening the
			// corpus. It is a disclosure, not a verified fact -- see claimScopeLoadFixtures.
			t.Logf("author: %s (self-reported) %s", fx.author, strings.Join(fx.notes, " | "))
			t.Logf("qualified %d, caught %d term(s): %v", q, len(hits), hits)
		})
	}
	t.Logf("%d known-bad fixture(s) re-proven caught by the live check", len(fixtures))
}

// TestBoundaryClaimScope_RatchetLeavesInnocentProseAlone is the corpus's other side, and it
// is not symmetry for its own sake.
//
// A ratchet applies one-sided pressure: every future miss argues for a wider subject test,
// and an always-true subject test satisfies every known-bad fixture at once. The arm above
// cannot tell the difference. These blocks can: prose that is CORRECT about the property,
// and prose that uses the forbidden words in their ordinary engineering senses while
// asserting nothing about a declared boundary. The second kind is the concrete reason this
// guard is not inverted -- an inverted trigger reds on it, and a guard that reds on
// legitimate prose gets deleted.
//
// 🔴 IT CARRIES ITS OWN 118-D1 ASSERTION, AND IT NEEDS IT. An innocent block that falls
// OUT OF SCOPE never reaches the prohibition, so it asserts nothing -- and a subject test
// that stopped firing puts EVERY innocent block out of scope at once, which is a silent
// pass over the whole corpus. That was measured, not reasoned: with the subject test forced
// always-false this arm was the one cell in the bite matrix that stayed green. So the count
// of fixtures that actually reached the prohibition is asserted. Out-of-scope blocks stay
// legal (the ordinary-prose control is one, deliberately: an inverted trigger is exactly
// what would drag it in), but they cannot be ALL of them.
func TestBoundaryClaimScope_RatchetLeavesInnocentProseAlone(t *testing.T) {
	fixtures := claimScopeLoadFixtures(t, claimScopeInnocentDir)

	// 🔴 THE HOLE-B COUNTER-WITNESS IS ASSERTED BY NAME, NOT MERELY CITED IN PROSE.
	//
	// The header explains that HOLE-B is a PRICE and that closing it reds THIS fixture. That
	// explanation is only load-bearing while the fixture exists, and the failure it guards
	// against runs in a specific order: someone closes HOLE-B, this fixture reds, and the
	// quickest way to green is to DELETE THE FIXTURE -- which also deletes the only evidence
	// that the trade was ever made, leaving prose pointing at nothing. Naming it here means
	// that deletion fails with a message saying why, instead of passing quietly.
	const holeBCounterWitness = "bare-letter-block-with-an-unrelated-sentence"
	found := false
	for _, fx := range fixtures {
		if fx.name == holeBCounterWitness {
			found = true
		}
	}
	require.True(t, found,
		"the HOLE-B counter-witness %q is GONE from %s. The header cites it as the reason HOLE-B is left open "+
			"deliberately -- it is a real comment with HOLE-B's exact shape, so any rule that closes HOLE-B reds "+
			"it. Without it the header's argument points at nothing and the next author reads a priced trade as "+
			"an unfinished one. If it was deleted to make a red go away, THAT RED WAS THE POINT.",
		holeBCounterWitness, claimScopeInnocentDir)

	inScope := 0
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			require.NotContains(t, fx.text, claimScopeAllow,
				"an innocent fixture must not need the opt-out; if it does, it is not innocent")
			q := claimScopeQualify(fx.text)
			if q == claimScopeOutOfScope {
				t.Logf("out of scope, never reaches the prohibition")
				return
			}
			inScope++
			require.Empty(t, claimScopeViolations(fx.text, q),
				"FALSE POSITIVE: this block is legitimate prose and the guard reds on it. A guard that reds on "+
					"correct writing is removed by the first person it inconveniences, and a removed guard "+
					"protects nothing.\n\nFixture %s:\n  %s",
				fx.name, strings.TrimSpace(fx.text))
		})
	}
	require.NotZero(t, inScope,
		"NONE of the %d innocent fixtures is in scope, so this arm ran the prohibition on nothing and passed. "+
			"Either the subject test stopped firing, or the corpus lost every block that exercises it",
		len(fixtures))
	t.Logf("innocent corpus: %d fixture(s), %d reached the prohibition", len(fixtures), inScope)
}
