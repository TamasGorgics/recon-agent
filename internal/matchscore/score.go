package matchscore

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth"
)

// Exclusion reasons reported in Result.Excluded. They document why a
// ground-truth scenario is not part of the recall denominator.
const (
	ReasonAmbiguous       = "ambiguous"
	ReasonEscalate        = "escalate"
	ReasonNoExternal      = "no_external_record"
	ReasonNoLedger        = "no_ledger_txn"
	ReasonNotExpectedLink = "not_expected_link"
)

// MatchGroup is one active match header with its external record and ledger
// legs. A 1:1 match has one ledger ID; a composite match has several.
type MatchGroup struct {
	MatchID      string
	ScopeID      string
	Rule         string
	External     match.RecordRef
	LedgerTxnIDs []string
}

// PassScore is one matcher rule's contribution. Precision is correct match
// groups divided by that rule's match groups. Recall is the number of required
// ground-truth links first recalled by this rule divided by all required links;
// summing per-pass recall gives overall recall because each active match has
// exactly one rule.
type PassScore struct {
	Pass           string
	Matches        int
	CorrectMatches int
	Precision      float64
	RequiredHits   int
	Recall         float64
}

// Result is one precision/recall evaluation.
type Result struct {
	Matches        int
	CorrectMatches int
	Precision      float64

	ExpectedLinks int
	RecalledLinks int
	Recall        float64

	PerPass  map[string]PassScore
	Excluded map[string]int
}

type association struct {
	scenarioID string
	ledgerSet  []string // sorted, de-duplicated
	required   bool
}

// Score compares active match groups with synthesis ground truth.
//
// Precision: a match group is correct when its external record belongs to a
// ground-truth scenario whose external and ledger sides both exist and whose
// ledger transaction set is exactly the group's leg set. The rule and the
// scenario's primary resolution do not matter: fee variance, FX rounding,
// duplicate and reference-typo scenarios still encode a real pair, and HD-3
// allows a partial_refund link. Precision is correct/active groups (1 when
// there are no active groups).
//
// Recall: the denominator is every scenario that allows "link" in
// expected_resolutions and has both sides. Ambiguous and escalate scenarios are
// excluded explicitly (and reported in Result.Excluded). A required scenario is
// recalled when at least one correct group covers its ledger set. Recall is
// recalled/required (0 when the denominator is empty).
func Score(groups []MatchGroup, truth synth.GroundTruth) Result {
	byExternal, expected, excluded := associations(truth)
	result := Result{
		Matches:       len(groups),
		ExpectedLinks: expected,
		PerPass:       make(map[string]PassScore),
		Excluded:      excluded,
	}

	passHits := make(map[string]map[string]struct{})
	recalled := make(map[string]struct{})
	for _, group := range groups {
		pass, ok := result.PerPass[group.Rule]
		if !ok {
			pass.Pass = group.Rule
		}
		pass.Matches++
		if assoc, ok := correctAssociation(group, byExternal); ok {
			result.CorrectMatches++
			pass.CorrectMatches++
			if assoc.required {
				if _, seen := recalled[assoc.scenarioID]; !seen {
					recalled[assoc.scenarioID] = struct{}{}
				}
				if passHits[group.Rule] == nil {
					passHits[group.Rule] = make(map[string]struct{})
				}
				passHits[group.Rule][assoc.scenarioID] = struct{}{}
			}
		}
		result.PerPass[group.Rule] = pass
	}

	result.Precision = ratio(result.CorrectMatches, result.Matches, true)
	result.RecalledLinks = len(recalled)
	result.Recall = ratio(result.RecalledLinks, result.ExpectedLinks, false)
	for rule, pass := range result.PerPass {
		pass.Precision = ratio(pass.CorrectMatches, pass.Matches, true)
		pass.RequiredHits = len(passHits[rule])
		pass.Recall = ratio(pass.RequiredHits, result.ExpectedLinks, false)
		result.PerPass[rule] = pass
	}
	return result
}

func ratio(numerator, denominator int, emptyOne bool) float64 {
	if denominator == 0 {
		if emptyOne {
			return 1
		}
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func associations(truth synth.GroundTruth) (map[string][]association, int, map[string]int) {
	byExternal := make(map[string][]association)
	excluded := make(map[string]int)
	expected := 0
	for _, scenario := range truth.Scenarios {
		switch {
		case scenario.Mode == "ambiguous":
			excluded[ReasonAmbiguous]++
			continue
		case contains(scenario.ExpectedResolutions, "escalate"):
			excluded[ReasonEscalate]++
			continue
		case len(scenario.ExternalRecords) == 0:
			excluded[ReasonNoExternal]++
			continue
		case len(scenario.LedgerTxnIDs) == 0:
			excluded[ReasonNoLedger]++
			continue
		}
		required := contains(scenario.ExpectedResolutions, "link")
		if !required {
			excluded[ReasonNotExpectedLink]++
		}
		assoc := association{
			scenarioID: scenario.ScenarioID,
			ledgerSet:  normalized(scenario.LedgerTxnIDs),
			required:   required,
		}
		for _, record := range scenario.ExternalRecords {
			key := refKey(record.Source, record.SourceRecordID)
			byExternal[key] = append(byExternal[key], assoc)
		}
		if required {
			expected++
		}
	}
	return byExternal, expected, excluded
}

func correctAssociation(group MatchGroup, byExternal map[string][]association) (association, bool) {
	for _, assoc := range byExternal[refKey(group.External.Source, group.External.SourceRecordID)] {
		if equalStrings(normalized(group.LedgerTxnIDs), assoc.ledgerSet) {
			return assoc, true
		}
	}
	return association{}, false
}

func refKey(source, id string) string { return source + "\x00" + id }

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func normalized(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	deduped := out[:0]
	for i, value := range out {
		if i == 0 || value != out[i-1] {
			deduped = append(deduped, value)
		}
	}
	return deduped
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Queryer is the read-only pgx subset needed to load active matches.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadActiveMatches returns every active match group, optionally restricted to
// one scope. The result is ordered by match id and each group's ledger IDs are
// sorted, so summaries are deterministic.
func LoadActiveMatches(ctx context.Context, q Queryer, scopeID string) ([]MatchGroup, error) {
	if q == nil {
		return nil, fmt.Errorf("matchscore: LoadActiveMatches requires a queryer")
	}
	var (
		rows pgx.Rows
		err  error
	)
	const columns = `
		SELECT m.id::text, m.scope_id::text, m.rule,
		       er.source, er.source_record_id, ml.ledger_txn_id::text
		  FROM matches m
		  JOIN external_records er ON er.id = m.external_record_id
		  LEFT JOIN match_legs ml ON ml.match_id = m.id AND ml.status = 'active'
		 WHERE m.status = 'active'`
	if scopeID == "" {
		rows, err = q.Query(ctx, columns+`
		  ORDER BY m.id, ml.ledger_txn_id`)
	} else {
		rows, err = q.Query(ctx, columns+`
		   AND m.scope_id = $1::uuid
		  ORDER BY m.id, ml.ledger_txn_id`, scopeID)
	}
	if err != nil {
		return nil, fmt.Errorf("matchscore: load active matches: %w", err)
	}
	defer rows.Close()

	var (
		groups []MatchGroup
		index  = make(map[string]int)
	)
	for rows.Next() {
		var (
			matchID, scope, rule, source, recordID string
			ledgerID                               *string
		)
		if err := rows.Scan(&matchID, &scope, &rule, &source, &recordID, &ledgerID); err != nil {
			return nil, fmt.Errorf("matchscore: scan active match: %w", err)
		}
		position, ok := index[matchID]
		if !ok {
			position = len(groups)
			index[matchID] = position
			groups = append(groups, MatchGroup{
				MatchID:  matchID,
				ScopeID:  scope,
				Rule:     rule,
				External: match.RecordRef{Source: source, SourceRecordID: recordID},
			})
		}
		if ledgerID != nil {
			groups[position].LedgerTxnIDs = append(groups[position].LedgerTxnIDs, *ledgerID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("matchscore: iterate active matches: %w", err)
	}
	for i := range groups {
		sort.Strings(groups[i].LedgerTxnIDs)
	}
	return groups, nil
}
