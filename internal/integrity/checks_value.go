package integrity

import (
	"context"
	"fmt"
)

// checkValueConservation implements the FIN-52 value equation per source and
// currency:
//
//	sum(active match legs)
//	+ sum(unmatched external records)
//	= sum(all ingested external records)
//
// Matched value is attributed through the match header's scope source/currency,
// while ingested and unmatched value use each external record's own
// source/currency. Keys are the union of ingested keys and active-match scope
// keys, so a cross-scope match fails instead of hiding value. All sums are
// signed (debit positive); there is no abs(), no per-currency netting, and no
// tolerance exemption.
func checkValueConservation(ctx context.Context, q queryer) (CheckResult, error) {
	rows, err := q.Query(ctx, `
		WITH ingested AS (
		    SELECT source, currency, sum(amount_minor)::text AS ingested_minor
		      FROM external_records
		     GROUP BY source, currency
		), unmatched AS (
		    SELECT er.source, er.currency, sum(er.amount_minor)::text AS unmatched_minor
		      FROM external_records er
		     WHERE NOT EXISTS (
		           SELECT 1 FROM matches m
		            WHERE m.external_record_id = er.id
		              AND m.status = 'active')
		     GROUP BY er.source, er.currency
		), matched AS (
		    SELECT s.source, s.currency, sum(ml.amount_minor)::text AS matched_minor
		      FROM matches m
		      JOIN match_legs ml ON ml.match_id = m.id AND ml.status = 'active'
		      JOIN reconciliation_scopes s ON s.id = m.scope_id
		     WHERE m.status = 'active'
		     GROUP BY s.source, s.currency
		), keys AS (
		    SELECT source, currency FROM ingested
		    UNION
		    SELECT source, currency FROM matched
		)
		SELECT k.source,
		       k.currency,
		       COALESCE(i.ingested_minor, '0'),
		       COALESCE(u.unmatched_minor, '0'),
		       COALESCE(m.matched_minor, '0')
		  FROM keys k
		  LEFT JOIN ingested i ON i.source = k.source AND i.currency = k.currency
		  LEFT JOIN unmatched u ON u.source = k.source AND u.currency = k.currency
		  LEFT JOIN matched m ON m.source = k.source AND m.currency = k.currency
		 ORDER BY k.source, k.currency`)
	if err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}
	defer rows.Close()

	var checked int64
	var problems []Problem
	for rows.Next() {
		var source, currency, ingestedText, unmatchedText, matchedText string
		if err := rows.Scan(&source, &currency, &ingestedText, &unmatchedText, &matchedText); err != nil {
			return CheckResult{}, &checkError{code: "query_failed", err: err}
		}
		ingested, err := parseInt64(ingestedText)
		if err != nil {
			return CheckResult{}, err
		}
		unmatched, err := parseInt64(unmatchedText)
		if err != nil {
			return CheckResult{}, err
		}
		matched, err := parseInt64(matchedText)
		if err != nil {
			return CheckResult{}, err
		}
		accounted, ok := addInt64(matched, unmatched)
		if !ok {
			return CheckResult{}, &checkError{code: "numeric_overflow"}
		}
		delta, ok := subInt64(ingested, accounted)
		if !ok {
			return CheckResult{}, &checkError{code: "numeric_overflow"}
		}
		checked++
		if delta == 0 {
			continue
		}
		problems = append(problems, Problem{
			Code:   CodeValueConservationMismatch,
			Entity: "source_currency",
			ID:     source + ":" + currency,
			Detail: fmt.Sprintf("ingested_minor=%d accounted_minor=%d delta_minor=%d",
				ingested, accounted, delta),
		})
	}
	if err := rows.Err(); err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}
	return finalizeCheckResult(CheckValueConservation, checked, 0, problems), nil
}
