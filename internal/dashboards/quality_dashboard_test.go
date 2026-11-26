// covers: FIN-42, FIN-M5-05-14, FIN-M5-05-15, FIN-M5-05-16, FIN-M5-05-17, FIN-M5-05-18
package dashboards_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/dashboards"
)

const (
	qualityDashboardRel   = "deploy/grafana/dashboards/agent-quality.json"
	qualityDashboardUID   = "recon-agent-quality"
	qualityDashboardTitle = "Recon Agent Quality"

	// m504OpsDashboardSHA256 freezes the M5-04 dashboard file across the M5-05
	// quality-dashboard wave: adding a second provisioned dashboard must not
	// alter Recon Ops. An intentional ops-dashboard change must update this
	// digest and TestM504DashboardJSON in the same commit.
	m504OpsDashboardSHA256 = "d8d55b4252c03b3a68e2c001fb4bf7a698cafb7ba95545ada5e6af996a5946fa"
)

// qualityPanelContract is the spec §4.2 panel inventory. Panel 6 has two
// targets (p50 and p95); every other panel has one.
type qualityPanelContract struct {
	id           int
	title        string
	panelType    string
	source       string
	valueColumns []string
	unit         string
	targets      int
	format       string
}

var qualityPanelContracts = []qualityPanelContract{
	{id: 1, title: "Accuracy by model / prompt", panelType: "timeseries", source: "v_eval_accuracy",
		valueColumns: []string{"accuracy"}, unit: "percentunit", targets: 1, format: "time_series"},
	{id: 2, title: "False-fix rate by model / prompt", panelType: "timeseries", source: "v_eval_false_fix",
		valueColumns: []string{"rate"}, unit: "percentunit", targets: 1, format: "time_series"},
	{id: 3, title: "Human override rate", panelType: "timeseries", source: "v_eval_override_rate",
		valueColumns: []string{"rate"}, unit: "percentunit", targets: 1, format: "time_series"},
	{id: 4, title: "Apply-failure rate", panelType: "timeseries", source: "v_eval_apply_failure_rate",
		valueColumns: []string{"rate"}, unit: "percentunit", targets: 1, format: "time_series"},
	{id: 5, title: "Average cost per case", panelType: "timeseries", source: "v_eval_cost_latency",
		valueColumns: []string{"avg_cost_usd"}, unit: "currencyUSD", targets: 1, format: "time_series"},
	{id: 6, title: "Latency p50 / p95", panelType: "timeseries", source: "v_eval_cost_latency",
		valueColumns: []string{"p50_latency_ms", "p95_latency_ms"}, unit: "ms", targets: 2, format: "time_series"},
	{id: 7, title: "Escalation reasons", panelType: "table", source: "v_eval_escalations",
		valueColumns: []string{"sum(count)"}, unit: "short", targets: 1, format: "table"},
	{id: 8, title: "Escalation rate", panelType: "timeseries", source: "v_eval_escalation_rate",
		valueColumns: []string{"rate"}, unit: "percentunit", targets: 1, format: "time_series"},
}

// disallowedMacros are forbidden by spec §4.2: only $__timeFilter(day) is
// allowed in the quality dashboard.
var disallowedMacros = []string{
	"$__timeFrom",
	"$__timeTo",
	"$__timeGroup",
	"$__interval",
	"$__rate_interval",
	"${",
}

// rawTableNames are the base/production tables the dashboard role cannot read
// and the dashboard must never query directly.
var rawTableNames = []string{
	"eval_runs",
	"eval_case_results",
	"approvals",
	"proposals",
	"agent_runs",
	"audit_log",
	"cases",
	"applied_proposals",
}

var fromJoinPattern = regexp.MustCompile(`(?i)\b(?:from|join)\s+([a-z_][a-z0-9_.]*)`)

type qualityRawTarget struct {
	Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
	LegendFormat string `json:"legendFormat"`
	RawSQL       string `json:"rawSql"`
}

type qualityRawPanel struct {
	ID      int                `json:"id"`
	Targets []qualityRawTarget `json:"targets"`
}

// TestM505QualityDashboardJSON is the FIN-M5-05-14..18 contract for
// agent-quality.json: identity/provisioning metadata, the exact eight-panel
// inventory, one-v_eval_*-view-per-query safety, allowed time macro, units,
// descriptions, and no credentials.
func TestM505QualityDashboardJSON(t *testing.T) {
	raw := readRepoFile(t, qualityDashboardRel)
	dashboard, err := dashboards.LoadDashboardFile(repoFile(t, qualityDashboardRel))
	if err != nil {
		t.Fatalf("load quality dashboard: %v", err)
	}

	if dashboard.UID != qualityDashboardUID {
		t.Errorf("uid = %q, want %q", dashboard.UID, qualityDashboardUID)
	}
	if dashboard.Title != qualityDashboardTitle {
		t.Errorf("title = %q, want %q", dashboard.Title, qualityDashboardTitle)
	}
	if dashboard.Timezone != "utc" {
		t.Errorf("timezone = %q, want utc", dashboard.Timezone)
	}
	if dashboard.Refresh != "5m" {
		t.Errorf("refresh = %q, want 5m", dashboard.Refresh)
	}
	if dashboard.Time.From != "now-14d" || dashboard.Time.To != "now" {
		t.Errorf("time = %+v, want now-14d..now", dashboard.Time)
	}
	if len(dashboard.Templating.List) != 0 {
		t.Errorf("templating variables = %d, want none", len(dashboard.Templating.List))
	}
	if len(dashboard.Inputs) != 0 {
		t.Errorf("__inputs = %d, want none", len(dashboard.Inputs))
	}
	if !containsString(dashboard.Tags, "recon") {
		t.Errorf("tags = %v, want a recon tag", dashboard.Tags)
	}

	// The folder is delivered by provider.yaml, not by the dashboard document.
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		t.Fatalf("parse dashboard top level: %v", err)
	}
	for _, key := range []string{"folder", "folderId", "folderUid", "meta"} {
		if _, ok := topLevel[key]; ok {
			t.Errorf("dashboard carries provider-owned %q key; folder must come from provider.yaml", key)
		}
	}
	if strings.Contains(string(raw), "${") {
		t.Error("dashboard contains a template variable or unexpanded provisioning placeholder")
	}

	var rawDocument struct {
		Panels []qualityRawPanel `json:"panels"`
	}
	if err := json.Unmarshal(raw, &rawDocument); err != nil {
		t.Fatalf("parse raw panel targets: %v", err)
	}
	rawPanels := make(map[int]qualityRawPanel, len(rawDocument.Panels))
	for _, panel := range rawDocument.Panels {
		rawPanels[panel.ID] = panel
	}

	panels := make(map[int]*dashboards.Panel, len(dashboard.Panels))
	for i := range dashboard.Panels {
		panel := &dashboard.Panels[i]
		if _, duplicate := panels[panel.ID]; duplicate {
			t.Fatalf("duplicate panel id %d", panel.ID)
		}
		panels[panel.ID] = panel
	}
	if len(dashboard.Panels) != len(qualityPanelContracts) || len(panels) != len(qualityPanelContracts) {
		t.Fatalf("panels = %d, want exactly %d with ids 1-8", len(dashboard.Panels), len(qualityPanelContracts))
	}

	for _, want := range qualityPanelContracts {
		panel, ok := panels[want.id]
		if !ok {
			t.Errorf("panel id %d is missing", want.id)
			continue
		}
		if panel.Title != want.title {
			t.Errorf("panel %d title = %q, want %q", want.id, panel.Title, want.title)
		}
		if panel.Type != want.panelType {
			t.Errorf("panel %d type = %q, want %q", want.id, panel.Type, want.panelType)
		}
		if strings.TrimSpace(panel.Description) == "" {
			t.Errorf("panel %d has an empty description", want.id)
		}
		if panel.Datasource.Type != "grafana-postgresql-datasource" ||
			panel.Datasource.UID != postgresDatasourceUID {
			t.Errorf("panel %d datasource = %+v, want grafana-postgresql-datasource/%s",
				want.id, panel.Datasource, postgresDatasourceUID)
		}
		if panel.GridPosition.W == 0 || panel.GridPosition.H == 0 {
			t.Errorf("panel %d has empty grid position: %+v", want.id, panel.GridPosition)
		}
		if len(panel.Targets) != want.targets {
			t.Errorf("panel %d targets = %d, want %d", want.id, len(panel.Targets), want.targets)
			continue
		}
		if got := qualityPanelUnit(t, panel); got != want.unit {
			t.Errorf("panel %d unit = %q, want %q", want.id, got, want.unit)
		}

		rawPanel, ok := rawPanels[want.id]
		if !ok {
			t.Errorf("panel %d has no raw JSON target", want.id)
			continue
		}
		if len(rawPanel.Targets) != want.targets {
			t.Errorf("panel %d raw targets = %d, want %d", want.id, len(rawPanel.Targets), want.targets)
			continue
		}

		refIDs := make(map[string]bool, len(panel.Targets))
		for i := range panel.Targets {
			target := &panel.Targets[i]
			rawTarget := rawPanel.Targets[i]
			if target.RefID == "" || refIDs[target.RefID] {
				t.Errorf("panel %d target %d refId = %q, want a unique non-empty refId", want.id, i, target.RefID)
			}
			refIDs[target.RefID] = true
			if rawTarget.Datasource.Type != "grafana-postgresql-datasource" ||
				rawTarget.Datasource.UID != postgresDatasourceUID {
				t.Errorf("panel %d target %s datasource = %+v, want grafana-postgresql-datasource/%s",
					want.id, target.RefID, rawTarget.Datasource, postgresDatasourceUID)
			}
			if target.Format != want.format {
				t.Errorf("panel %d target %s format = %q, want %q", want.id, target.RefID, target.Format, want.format)
			}
			if strings.TrimSpace(rawTarget.LegendFormat) == "" && want.panelType == "timeseries" {
				t.Errorf("panel %d target %s has an empty legendFormat", want.id, target.RefID)
			}
			assertQualityPanelSQL(t, want, target.RawSQL, want.valueColumns[i])
		}
	}

	// Description boundaries that the panel contract depends on.
	requireDescription(t, panels, 1, "injection")
	requireDescription(t, panels, 2, "injection")
	requireDescription(t, panels, 3, "edit")
	requireDescription(t, panels, 4, "apply_failed")
	requireDescription(t, panels, 5, "per-case")
	requireDescription(t, panels, 6, "p50")
	requireDescription(t, panels, 6, "p95")
	requireDescription(t, panels, 6, "nearest-rank")
	requireDescription(t, panels, 7, "reason")
	requireDescription(t, panels, 8, "cohort")
	requireDescription(t, panels, 8, "created")
	requireDescription(t, panels, 8, "right-censored")
	requireDescription(t, panels, 8, "NULL")

	// Panel 6 must expose p50 and p95 as separate targets with distinct legends.
	panel6Targets := rawPanels[6].Targets
	if len(panel6Targets) == 2 {
		if !strings.Contains(panel6Targets[0].LegendFormat, "p50") ||
			!strings.Contains(panel6Targets[1].LegendFormat, "p95") {
			t.Errorf("panel 6 legends = %q/%q, want p50/p95 distinguished",
				panel6Targets[0].LegendFormat, panel6Targets[1].LegendFormat)
		}
	}

	assertNoDashboardSecrets(t, raw)
}

// assertQualityPanelSQL enforces spec §4.2 query safety: exactly one v_eval_*
// source, the $__timeFilter(day) macro only, no raw table, and no COALESCE on
// a ratio.
func assertQualityPanelSQL(t *testing.T, want qualityPanelContract, sql, valueColumn string) {
	t.Helper()
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		t.Errorf("panel %d has an empty rawSql", want.id)
		return
	}
	if !strings.Contains(trimmed, "$__timeFilter(day)") {
		t.Errorf("panel %d SQL does not filter with $__timeFilter(day): %q", want.id, trimmed)
	}
	if got := strings.Count(trimmed, "$__timeFilter(day)"); got != 1 {
		t.Errorf("panel %d uses $__timeFilter(day) %d times, want exactly 1: %q", want.id, got, trimmed)
	}
	remaining := strings.ReplaceAll(trimmed, "$__timeFilter(day)", "")
	if strings.Contains(remaining, "$__") {
		t.Errorf("panel %d SQL uses a disallowed Grafana macro: %q", want.id, trimmed)
	}
	for _, macro := range disallowedMacros {
		if strings.Contains(trimmed, macro) {
			t.Errorf("panel %d SQL uses disallowed macro %q: %q", want.id, macro, trimmed)
		}
	}

	matches := fromJoinPattern.FindAllStringSubmatch(trimmed, -1)
	if len(matches) != 1 {
		t.Errorf("panel %d SQL references %d FROM/JOIN sources, want exactly 1: %q", want.id, len(matches), trimmed)
	}
	for _, match := range matches {
		table := match[1]
		if !strings.HasPrefix(table, "v_eval_") {
			t.Errorf("panel %d SQL reads non-eval source %q: %q", want.id, table, trimmed)
		}
	}
	if !strings.Contains(trimmed, "FROM "+want.source) {
		t.Errorf("panel %d SQL does not read %s: %q", want.id, want.source, trimmed)
	}

	for _, rawTable := range rawTableNames {
		if strings.Contains(trimmed, rawTable) {
			t.Errorf("panel %d SQL names raw table %q: %q", want.id, rawTable, trimmed)
		}
	}
	if strings.Contains(strings.ToUpper(trimmed), "COALESCE") {
		t.Errorf("panel %d SQL wraps a ratio in COALESCE: %q", want.id, trimmed)
	}
	if !strings.Contains(trimmed, valueColumn) {
		t.Errorf("panel %d SQL does not select %q: %q", want.id, valueColumn, trimmed)
	}
}

// TestM505ProviderDiscoversBothDashboards pins the existing provider path and
// proves the provider discovers exactly the M5-04 and M5-05 dashboards in
// folder Recon, with Recon Ops byte-for-byte unchanged.
func TestM505ProviderDiscoversBothDashboards(t *testing.T) {
	providerDoc := loadYAML[providerDocument](t, "deploy/grafana/provisioning/dashboards/provider.yaml")
	if providerDoc.APIVersion != 1 || len(providerDoc.Providers) != 1 {
		t.Fatalf("provider document = apiVersion %d, %d providers", providerDoc.APIVersion, len(providerDoc.Providers))
	}
	provider := providerDoc.Providers[0]
	if provider.Folder != "Recon" {
		t.Errorf("provider folder = %q, want Recon", provider.Folder)
	}
	if provider.Type != "file" || provider.Options.Path != "/var/lib/grafana/dashboards" {
		t.Errorf("provider delivery = type %q path %q, want file /var/lib/grafana/dashboards",
			provider.Type, provider.Options.Path)
	}
	if provider.Options.FoldersFromFilesStructure {
		t.Error("provider uses foldersFromFilesStructure; both dashboards must land in the single Recon folder")
	}

	// The provisioned path is the compose bind mount of deploy/grafana/dashboards.
	compose := loadYAML[composeDocument](t, "compose.yaml")
	grafana, ok := compose.Services["grafana"]
	if !ok {
		t.Fatal("compose is missing the grafana service")
	}
	requireMount(t, "grafana", grafana.Volumes, "./deploy/grafana/dashboards:/var/lib/grafana/dashboards:ro")

	entries, err := os.ReadDir(repoFile(t, "deploy/grafana/dashboards"))
	if err != nil {
		t.Fatalf("read dashboards directory: %v", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	wantFiles := []string{"agent-quality.json", "ops.json"}
	if strings.Join(files, ",") != strings.Join(wantFiles, ",") {
		t.Fatalf("provisioned dashboard files = %v, want exactly %v", files, wantFiles)
	}

	byUID := map[string]string{}
	for _, file := range files {
		path := "deploy/grafana/dashboards/" + file
		dashboard, err := dashboards.LoadDashboardFile(repoFile(t, path))
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		if _, duplicate := byUID[dashboard.UID]; duplicate {
			t.Errorf("duplicate provisioned dashboard UID %q", dashboard.UID)
		}
		byUID[dashboard.UID] = file
	}
	if byUID[qualityDashboardUID] != "agent-quality.json" {
		t.Errorf("uid %q maps to %q, want agent-quality.json", qualityDashboardUID, byUID[qualityDashboardUID])
	}
	if byUID["recon-ops"] != "ops.json" {
		t.Errorf("uid recon-ops maps to %q, want ops.json", byUID["recon-ops"])
	}

	// M5-04 dashboard: unchanged identity and six-panel inventory. The exact
	// SQL contract is already frozen by TestM504DashboardJSON.
	ops := loadDashboardFile(t, "deploy/grafana/dashboards/ops.json")
	if ops.UID != "recon-ops" || ops.Title != "Recon Ops" {
		t.Errorf("ops identity = %q/%q, want recon-ops/Recon Ops", ops.UID, ops.Title)
	}
	if len(ops.Panels) != 6 {
		t.Errorf("ops panels = %d, want the unchanged six-panel M5-04 inventory", len(ops.Panels))
	}
	for id := 1; id <= 6; id++ {
		if ops.PanelByID(id) == nil {
			t.Errorf("ops panel id %d is missing", id)
		}
	}
	sum := sha256.Sum256(readRepoFile(t, "deploy/grafana/dashboards/ops.json"))
	if got := hex.EncodeToString(sum[:]); got != m504OpsDashboardSHA256 {
		t.Errorf("ops.json = %s, want the frozen %s; M5-05 W2 must not edit Recon Ops",
			got, m504OpsDashboardSHA256)
	}

	quality := loadDashboardFile(t, qualityDashboardRel)
	if quality.UID != qualityDashboardUID || quality.Title != qualityDashboardTitle {
		t.Errorf("quality identity = %q/%q, want %q/%q",
			quality.UID, quality.Title, qualityDashboardUID, qualityDashboardTitle)
	}
}

func loadDashboardFile(t *testing.T, rel string) *dashboards.Dashboard {
	t.Helper()
	dashboard, err := dashboards.LoadDashboardFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("load %s: %v", rel, err)
	}
	return dashboard
}

func qualityPanelUnit(t *testing.T, panel *dashboards.Panel) string {
	t.Helper()
	var config struct {
		Defaults struct {
			Unit string `json:"unit"`
		} `json:"defaults"`
	}
	if len(panel.FieldConfig) == 0 {
		return ""
	}
	if err := json.Unmarshal(panel.FieldConfig, &config); err != nil {
		t.Fatalf("parse panel %d fieldConfig: %v", panel.ID, err)
	}
	return config.Defaults.Unit
}

func requireDescription(t *testing.T, panels map[int]*dashboards.Panel, id int, want string) {
	t.Helper()
	panel := panels[id]
	if panel == nil {
		t.Errorf("panel %d is missing", id)
		return
	}
	if !strings.Contains(panel.Description, want) {
		t.Errorf("panel %d description is missing %q: %q", id, want, panel.Description)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
