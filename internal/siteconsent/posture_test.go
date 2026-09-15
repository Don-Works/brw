package siteconsent

import (
	"strings"
	"testing"
)

// Merge is what makes the posture at /health the whole chain's answer rather
// than one process's. Every axis is an OR because either end can be the one
// that stops and asks, and an unattended caller has to hear the stricter of the
// two.
func TestMergeTakesTheStricterAnswerOnEveryAxis(t *testing.T) {
	quiet := Posture{}
	prompting := Posture{Enabled: true, Interactive: true}
	confirming := Posture{Enabled: true, ConfirmActions: true}

	for name, merged := range map[string]Posture{
		"the prompter is upstream":   quiet.Merge(prompting),
		"the prompter is downstream": prompting.Merge(quiet),
	} {
		if !merged.Interactive || !merged.Enabled {
			t.Errorf("%s: merged to %+v, so a caller is told nothing in the chain would ask", name, merged)
		}
		if merged.Unknown {
			t.Errorf("%s: two definite answers merged to an unknown one: %+v", name, merged)
		}
	}
	if got := quiet.Merge(confirming); !got.ConfirmActions {
		t.Errorf("a confirmation gate on one hop disappeared: %+v", got)
	}
	if got := quiet.Merge(quiet); got != (Posture{}) {
		t.Errorf("two daemons with no gate at all merged to %+v", got)
	}
}

// A hop that could not be asked is not a hop that said no: "unknown" survives
// the merge and carries the reason, because `brw run` treats it the way it
// treats "yes".
func TestMergeKeepsAnUnknownHopAndItsReason(t *testing.T) {
	known := Posture{Enabled: true}
	unknown := UnreadablePosture("the daemon at http://127.0.0.1:17310 reports no consent posture")

	merged := known.Merge(unknown)
	if !merged.Unknown {
		t.Fatalf("an unreadable hop was dropped: %+v", merged)
	}
	if !strings.Contains(merged.Reason, "17310") {
		t.Errorf("the merged posture does not name what could not be asked: %q", merged.Reason)
	}
	if !merged.Enabled {
		t.Errorf("the known half of the chain was lost: %+v", merged)
	}

	// Two of them name both, so an operator fixing this knows how many hops are
	// dark rather than only the first.
	both := UnreadablePosture("first hop").Merge(UnreadablePosture("second hop"))
	for _, want := range []string{"first hop", "second hop"} {
		if !strings.Contains(both.Reason, want) {
			t.Errorf("the merged reason does not name %q: %q", want, both.Reason)
		}
	}
}

// A daemon with no consent gate answers definitely rather than unknown: "there
// is nothing here that would ask you" is exactly what an unattended run needs
// to hear, and a nil guard is how brwd spells it.
func TestANilGuardIsADefiniteAnswer(t *testing.T) {
	var guard *Guard
	posture := guard.Posture()
	if posture.Unknown {
		t.Fatalf("a daemon started without --site-consent reports an unknown posture: %+v", posture)
	}
	if posture.Enabled || posture.Interactive || posture.ConfirmActions {
		t.Fatalf("a daemon with no consent gate reports one: %+v", posture)
	}
}
