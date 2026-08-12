package workflow

// CONS-01 — SubWorkflowChildID and FanOutChildID are a STABLE CONTRACT.
//
// Both were unexported with no exported equivalent, which made two documented patterns
// unfollowable: WithCollectPartial tells a consumer to inspect a failed branch's child
// journal "by its deterministic ID", and the parked pattern tells the host to run the
// child itself without saying under what WorkflowID.
//
// Exporting them is a promise that downstream systems may recompute the ID. The golden
// digests below are what make that promise falsifiable: any change to the hash framing
// reddens this file. That is the point — the construction must not change across
// versions without a deliberate, documented break, exactly as IdempotencyKey states.
//
// The length prefixes are a COLLISION GUARD, not incidental framing. The ("ab","c") vs
// ("a","bc") pairs below are the cases a naive concatenation gets wrong, and they are
// the reason a consumer must call these rather than reimplement them.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubWorkflowChildID_GoldenContract(t *testing.T) {
	golden := []struct {
		parentID string
		nodeName string
		want     string
	}{
		{"parent", "sub", "sub:9d15685818bff6a17c1b9cefaf12031dd0cc532dd9ac47015b3c84abaff1d407"},
		{"ab", "c", "sub:6d38234db36d6dcc6ff6702b434e13bcdad84fa7a0aed399b15b83a5fe49d721"},
		{"a", "bc", "sub:22a5c7045bc36a32e64f063ff4e7946d8a663238bf27e4f5b32713d987ec51ac"},
		{"", "", "sub:af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc"},
	}
	for _, g := range golden {
		assert.Equal(t, g.want, SubWorkflowChildID(g.parentID, g.nodeName),
			"the child-ID construction is a published contract — a change here breaks every "+
				"downstream system that recomputes it")
	}
}

func TestFanOutChildID_GoldenContract(t *testing.T) {
	golden := []struct {
		parentID string
		nodeName string
		index    int
		want     string
	}{
		{"parent", "fan", 0, "fan:4e1cb7b52fd53b4e7da5193ac8bad34ee465280a7eecfc368f52d4035720170a"},
		{"parent", "fan", 1, "fan:54f6bc86fc504303c24c48606282188c7b66e2a479008c044a00df46bb20a92b"},
		{"ab", "c", 0, "fan:03bbec117707a23f74de53c77d0ac02d9db8dcb0cb4947584373fdcac2a5dfae"},
		{"a", "bc", 0, "fan:4bba87d4404fb83649a19d4ee7df22a3405c514ad8d0638797e4988e0b7cd541"},
	}
	for _, g := range golden {
		assert.Equal(t, g.want, FanOutChildID(g.parentID, g.nodeName, g.index),
			"the fan-out branch-ID construction is a published contract")
	}
}

// TestChildID_LengthPrefixPreventsCollision is the property the golden values encode.
// It is stated separately because it is the REASON for the framing: drop the length
// prefix and these pairs collide, silently pointing two different children at one
// WorkflowID.
func TestChildID_LengthPrefixPreventsCollision(t *testing.T) {
	assert.NotEqual(t, SubWorkflowChildID("ab", "c"), SubWorkflowChildID("a", "bc"),
		`("ab","c") and ("a","bc") must not collide — the length prefix is what separates them`)

	assert.NotEqual(t, FanOutChildID("ab", "c", 0), FanOutChildID("a", "bc", 0),
		`("ab","c",0) and ("a","bc",0) must not collide`)

	// The index must be part of the digest, or every branch of one node shares an ID.
	assert.NotEqual(t, FanOutChildID("p", "n", 0), FanOutChildID("p", "n", 1),
		"branch index must be folded into the digest")

	// The two namespaces must never produce the same ID for the same (parent, node).
	assert.NotEqual(t, SubWorkflowChildID("p", "n"), FanOutChildID("p", "n", 0),
		`"sub:" and "fan:" children must be distinguishable`)
}

// TestChildID_Deterministic pins the resume-stability the engine itself relies on: a
// re-drive must find the SAME child, not spawn a second one.
func TestChildID_Deterministic(t *testing.T) {
	require.Equal(t, SubWorkflowChildID("p", "sub"), SubWorkflowChildID("p", "sub"))
	require.Equal(t, FanOutChildID("p", "fan", 7), FanOutChildID("p", "fan", 7))
}
