package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// RosterProposalStatus is where a RosterProposal stands.
type RosterProposalStatus string

const (
	// RosterPending waits for the reader.
	RosterPending RosterProposalStatus = "pending"
	// RosterAccepted was applied.
	RosterAccepted RosterProposalStatus = "accepted"
	// RosterSkipped was declined by the reader.
	RosterSkipped RosterProposalStatus = "skipped"
	// RosterRuledOut is a pair of names the review found to be different
	// people, or couldn't settle - never shown, kept so the pair isn't
	// asked about again.
	RosterRuledOut RosterProposalStatus = "ruled_out"
)

// RosterProposalSource is who proposed a RosterProposal.
type RosterProposalSource string

const (
	// RosterFromRule was found by a rule, without reading the text.
	RosterFromRule RosterProposalSource = "rule"
	// RosterFromLLM is the review pass's verdict on a pair of names.
	RosterFromLLM RosterProposalSource = "llm"
	// RosterFromAgent was filed through the API.
	RosterFromAgent RosterProposalSource = "agent"
)

// RosterProposal is one suggested fix to a character roster (see
// httpapi.ReviewRoster). Kind is a speakerattr.RosterFixKind.
type RosterProposal struct {
	ID    string
	Scope string
	Kind  string
	// SubjectID/Subject is the character it changes, by id and by the name
	// it had when proposed; TargetID/Target the one it is merged into, or
	// whose name an alias collides with ("" when none).
	SubjectID string
	Subject   string
	TargetID  string
	Target    string
	// Detail is the alias to remove.
	Detail string
	Reason string
	// Evidence is the passages that show it.
	Evidence []string
	Source   RosterProposalSource
	Status   RosterProposalStatus
	// DedupeKey names what the proposal is about; a scope holds one row
	// per key.
	DedupeKey string
	CreatedAt int64
	DecidedAt int64
}

const rosterProposalColumns = `id, scope, kind, subject_id, subject, target_id, target, detail, reason, CAST(evidence AS VARCHAR), source, status, dedupe_key, created_at, decided_at`

func scanRosterProposal(row interface{ Scan(...any) error }) (RosterProposal, error) {
	var p RosterProposal
	var evidence string
	if err := row.Scan(&p.ID, &p.Scope, &p.Kind, &p.SubjectID, &p.Subject, &p.TargetID, &p.Target, &p.Detail, &p.Reason, &evidence, &p.Source, &p.Status, &p.DedupeKey, &p.CreatedAt, &p.DecidedAt); err != nil {
		return RosterProposal{}, err
	}
	_ = json.Unmarshal([]byte(evidence), &p.Evidence) // unreadable evidence reads as none
	return p, nil
}

// InsertRosterProposal stores p (its ID and CreatedAt filled in when
// blank) unless its scope already has a row with its DedupeKey; inserted
// reports which. The one row it replaces is a RosterRuledOut one, with a
// proposal that isn't: a pair the review couldn't settle can still be
// proposed by whoever can.
func (s *SQLiteStore) InsertRosterProposal(p RosterProposal) (inserted bool, err error) {
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	if p.Status == "" {
		p.Status = RosterPending
	}
	if p.Evidence == nil {
		p.Evidence = []string{}
	}
	evidence, err := json.Marshal(p.Evidence)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`INSERT INTO roster_proposals (id, scope, kind, subject_id, subject, target_id, target, detail, reason, evidence, source, status, dedupe_key, created_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (scope, dedupe_key) DO UPDATE SET
			id = excluded.id, kind = excluded.kind, subject_id = excluded.subject_id, subject = excluded.subject, target_id = excluded.target_id,
			target = excluded.target, detail = excluded.detail, reason = excluded.reason, evidence = excluded.evidence, source = excluded.source,
			status = excluded.status, created_at = excluded.created_at, decided_at = excluded.decided_at
		WHERE roster_proposals.status = 'ruled_out' AND excluded.status != 'ruled_out'`,
		p.ID, p.Scope, p.Kind, p.SubjectID, p.Subject, p.TargetID, p.Target, p.Detail, p.Reason, string(evidence), string(p.Source), string(p.Status), p.DedupeKey, p.CreatedAt, p.DecidedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListRosterProposals returns scope's proposals of every status, oldest
// first.
func (s *SQLiteStore) ListRosterProposals(scope string) ([]RosterProposal, error) {
	rows, err := s.db.Query(`SELECT `+rosterProposalColumns+` FROM roster_proposals WHERE scope = ? ORDER BY created_at ASC, rowid ASC`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RosterProposal
	for rows.Next() {
		p, err := scanRosterProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetRosterProposal is a by-id lookup, nil when there is none.
func (s *SQLiteStore) GetRosterProposal(id string) (*RosterProposal, error) {
	p, err := scanRosterProposal(s.db.QueryRow(`SELECT `+rosterProposalColumns+` FROM roster_proposals WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetRosterProposalStatus records the decision on a proposal.
func (s *SQLiteStore) SetRosterProposalStatus(id string, status RosterProposalStatus) error {
	_, err := s.db.Exec(`UPDATE roster_proposals SET status = ?, decided_at = ? WHERE id = ?`, string(status), time.Now().Unix(), id)
	return err
}
