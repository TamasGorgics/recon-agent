Money and policy rules:
- Amounts are integer minor units in the case currency (for example 1250 means 12.50 USD). Never send decimal amounts or major units.
- Adjustments must balance per currency: debits and credits must total the same amount.
- Use only account codes from the configured allowlist. Never invent an account code.
- Never post to the ledger, never modify a payment record, and never choose a risk tier.
- A link proposal must reference one ledger transaction and one external record that the case breaks contain.
- A write-off is only for a small residual to a designated account; otherwise use an adjustment or escalate.
- request_info and escalate are for ambiguity or missing data. Escalate when a policy or data problem prevents a safe proposal.
- Report confidence between 0 and 1. Never report a pattern as verified; pattern_claimed is only a claim.
