package report

import (
	"strings"
	"testing"
)

func TestParseRunConfigWhitelistAndMoneyFormat(t *testing.T) {
	config, err := parseRunConfig(`{
		"aborted": true,
		"abort_reason": "cost_cap",
		"cost_cap": "0.5",
		"pricing": {"api_key": "oc_sk_secret"},
		"eval": {"dsn": "postgres://user:pass@db/x"}
	}`)
	if err != nil {
		t.Fatalf("parseRunConfig: %v", err)
	}
	if !config.aborted || config.abortReason != "cost_cap" {
		t.Fatalf("config = %+v", config)
	}
	if config.costCap == nil || *config.costCap != "0.500000" {
		t.Fatalf("cost cap = %v, want 0.500000", config.costCap)
	}
}

func TestParseRunConfigRejectsBadCostCap(t *testing.T) {
	if _, err := parseRunConfig(`{"cost_cap":"not-money"}`); err == nil ||
		!strings.Contains(err.Error(), "cost_cap") {
		t.Fatalf("bad cost cap error = %v", err)
	}
	if _, err := parseRunConfig(`{"cost_cap":"-1"}`); err == nil ||
		!strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative cost cap error = %v", err)
	}
}

func TestParseRunConfigEmpty(t *testing.T) {
	config, err := parseRunConfig("")
	if err != nil {
		t.Fatalf("parseRunConfig empty: %v", err)
	}
	if config.aborted || config.abortReason != "" || config.costCap != nil {
		t.Fatalf("config = %+v", config)
	}
}
