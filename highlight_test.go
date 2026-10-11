package search

import "testing"

// A headline with nothing highlighted is not a snippet. Postgres returns an
// opening fragment when the term matched a column the snippet is not drawn
// from, and presenting that as the matched text says "this is why it matched"
// about text that is not why it matched.
func TestAnUnhighlightedHeadlineIsNotASnippet(t *testing.T) {
	if got := Snippet("Ranking and headlines … A note about something"); got != "" {
		t.Fatalf("Snippet() = %q for a headline with no match, want empty", got)
	}
	if got := Snippet("a " + startSel + "match" + stopSel + " here"); got == "" {
		t.Fatal("a highlighted headline was dropped")
	}
}
