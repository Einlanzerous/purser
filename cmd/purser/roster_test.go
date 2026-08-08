package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/invite"
)

// The JSON renderer owes its reader the hidden count for the same reason the
// table does, and more so: stderr is exactly what a `| jq` pipeline discards,
// so without this field an empty `people` reads as "nobody has Lyceum" when the
// truth is "nobody has Lyceum any more".
func TestRosterDTO_CarriesTheHiddenCount(t *testing.T) {
	// The shape that motivates it: every match filtered out for being stale.
	res := &invite.RosterResult{Hidden: 2}

	dto := newRosterDTO(res)
	if dto.HiddenAccounts != 2 {
		t.Errorf("HiddenAccounts = %d, want 2", dto.HiddenAccounts)
	}

	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	// An empty roster must still encode as [], not null: a consumer iterating
	// the field shouldn't have to special-case one of them.
	if !strings.Contains(string(encoded), `"people":[]`) {
		t.Errorf("an empty roster should encode as []:\n%s", encoded)
	}
	if !strings.Contains(string(encoded), `"hidden_accounts":2`) {
		t.Errorf("the hidden count is missing from the document:\n%s", encoded)
	}

	// Zero must be present rather than omitted — "the field is absent" and "the
	// value is zero" being the same wire form is the ambiguity it exists to
	// remove, so omitempty on this field is a regression.
	encoded, err = json.Marshal(newRosterDTO(&invite.RosterResult{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"hidden_accounts":0`) {
		t.Errorf("hidden_accounts must be emitted even at zero:\n%s", encoded)
	}
}
