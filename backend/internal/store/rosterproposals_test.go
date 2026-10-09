package store

import "testing"

func TestRosterProposals(t *testing.T) {
	s := openTestStore(t)
	p := RosterProposal{Scope: "series:X", Kind: "merge", SubjectID: "a", Subject: "Lars", TargetID: "b", Target: "Lars Machmüller",
		Evidence: []string{"Lars Machmüller waved; Lars was like that."}, Source: RosterFromLLM, Status: RosterRuledOut, DedupeKey: "pair:a:b"}
	if inserted, err := s.InsertRosterProposal(p); err != nil || !inserted {
		t.Fatalf("insert: %v, %v", inserted, err)
	}
	// The same key again changes nothing while both are ruled out...
	if inserted, err := s.InsertRosterProposal(p); err != nil || inserted {
		t.Fatalf("second ruled-out insert: %v, %v", inserted, err)
	}
	// ...but a real proposal replaces a ruled-out row.
	p.Status, p.Source, p.Reason = RosterPending, RosterFromAgent, "read it"
	if inserted, err := s.InsertRosterProposal(p); err != nil || !inserted {
		t.Fatalf("pending over ruled-out: %v, %v", inserted, err)
	}
	// And a pending one isn't replaced by anything.
	p.Reason = "again"
	if inserted, err := s.InsertRosterProposal(p); err != nil || inserted {
		t.Fatalf("pending over pending: %v, %v", inserted, err)
	}
	// Another scope has its own keys.
	other := p
	other.Scope = "book:1"
	if inserted, err := s.InsertRosterProposal(other); err != nil || !inserted {
		t.Fatalf("other scope: %v, %v", inserted, err)
	}

	rows, err := s.ListRosterProposals("series:X")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %d rows, %v", len(rows), err)
	}
	got := rows[0]
	if got.Status != RosterPending || got.Source != RosterFromAgent || got.Reason != "read it" || len(got.Evidence) != 1 || got.CreatedAt == 0 {
		t.Errorf("row = %+v", got)
	}
	if err := s.SetRosterProposalStatus(got.ID, RosterSkipped); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetRosterProposal(got.ID); again == nil || again.Status != RosterSkipped || again.DecidedAt == 0 {
		t.Errorf("after skip: %+v", again)
	}
	if missing, err := s.GetRosterProposal("nope"); err != nil || missing != nil {
		t.Errorf("missing: %+v, %v", missing, err)
	}
}
