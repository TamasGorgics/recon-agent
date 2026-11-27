package integrity

import (
	"context"
	"fmt"
	"strconv"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// checkAuditChain verifies the full audit hash chain from genesis through the
// production app role, read-only. An empty chain passes. Problems contain only
// the finding kind and sequence (plus the head sequence where relevant), never
// an audit payload. Verify never appends to the chain.
func checkAuditChain(ctx context.Context, q queryer) (CheckResult, error) {
	report, err := audit.Verify(ctx, q, 1)
	if err != nil {
		return CheckResult{}, &checkError{code: "verify_failed", err: err}
	}

	problems := make([]Problem, 0, len(report.Problems))
	for _, finding := range report.Problems {
		detail := fmt.Sprintf("kind=%s seq=%d", finding.Kind, finding.Seq)
		id := finding.Seq
		switch finding.Kind {
		case audit.ProblemHeadMismatch:
			id = report.HeadSeq
			detail += fmt.Sprintf(" head_seq=%d", report.HeadSeq)
		case audit.ProblemTruncatedTail:
			detail += fmt.Sprintf(" head_seq=%d", report.HeadSeq)
		}
		problems = append(problems, Problem{
			Code:   CodeAuditChainProblem,
			Entity: "audit_log",
			ID:     strconv.FormatInt(id, 10),
			Detail: detail,
		})
	}
	return finalizeCheckResult(CheckAuditChain, int64(report.Checked), 0, problems), nil
}
