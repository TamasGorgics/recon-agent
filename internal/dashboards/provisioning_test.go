// covers: FIN-42
package dashboards_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/TamasGorgics/recon-agent/internal/dashboards"
)

const (
	postgresDatasourceUID   = "recon-postgres"
	prometheusDatasourceUID = "recon-prometheus"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file with runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", rel)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", rel, err)
	}
	return path
}

func readRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

type datasourceDocument struct {
	APIVersion  int          `yaml:"apiVersion"`
	Datasources []datasource `yaml:"datasources"`
}

type datasource struct {
	Name           string            `yaml:"name"`
	UID            string            `yaml:"uid"`
	Type           string            `yaml:"type"`
	Access         string            `yaml:"access"`
	URL            string            `yaml:"url"`
	Database       string            `yaml:"database"`
	User           string            `yaml:"user"`
	IsDefault      bool              `yaml:"isDefault"`
	Editable       bool              `yaml:"editable"`
	JSONData       map[string]any    `yaml:"jsonData"`
	SecureJSONData map[string]string `yaml:"secureJsonData"`
}

type providerDocument struct {
	APIVersion int        `yaml:"apiVersion"`
	Providers  []provider `yaml:"providers"`
}

type provider struct {
	Name                  string          `yaml:"name"`
	OrgID                 int             `yaml:"orgId"`
	Folder                string          `yaml:"folder"`
	Type                  string          `yaml:"type"`
	DisableDeletion       bool            `yaml:"disableDeletion"`
	AllowUIUpdates        bool            `yaml:"allowUiUpdates"`
	UpdateIntervalSeconds int             `yaml:"updateIntervalSeconds"`
	Options               providerOptions `yaml:"options"`
}

type providerOptions struct {
	Path                      string `yaml:"path"`
	FoldersFromFilesStructure bool   `yaml:"foldersFromFilesStructure"`
}

type prometheusDocument struct {
	Global struct {
		ScrapeInterval string `yaml:"scrape_interval"`
	} `yaml:"global"`
	ScrapeConfigs []struct {
		JobName       string `yaml:"job_name"`
		StaticConfigs []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

type composeDocument struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	Image       string                    `yaml:"image"`
	Command     []string                  `yaml:"command"`
	Ports       []string                  `yaml:"ports"`
	Volumes     []string                  `yaml:"volumes"`
	Environment map[string]string         `yaml:"environment"`
	DependsOn   map[string]composeDepends `yaml:"depends_on"`
	Healthcheck *composeHealthcheck       `yaml:"healthcheck"`
}

type composeDepends struct {
	Condition string `yaml:"condition"`
}

type composeHealthcheck struct {
	Test     []string `yaml:"test"`
	Interval string   `yaml:"interval"`
	Timeout  string   `yaml:"timeout"`
	Retries  int      `yaml:"retries"`
}

func loadYAML[T any](t *testing.T, rel string) T {
	t.Helper()
	var doc T
	if err := yaml.Unmarshal(readRepoFile(t, rel), &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return doc
}

// TestM504DashboardJSON pins the frozen dashboard identity, panel contract,
// exact panel SQL/PromQL, display units, canonical aging labels, and the
// absence of credentials or unsupported Grafana macros.
func TestM504DashboardJSON(t *testing.T) {
	path := repoFile(t, "deploy/grafana/dashboards/ops.json")
	dashboard, err := dashboards.LoadDashboardFile(path)
	if err != nil {
		t.Fatalf("load dashboard: %v", err)
	}

	if dashboard.UID != "recon-ops" {
		t.Errorf("uid = %q, want recon-ops", dashboard.UID)
	}
	if dashboard.Title != "Recon Ops" {
		t.Errorf("title = %q, want Recon Ops", dashboard.Title)
	}
	if dashboard.Timezone != "utc" {
		t.Errorf("timezone = %q, want utc", dashboard.Timezone)
	}
	if dashboard.Refresh != "30s" {
		t.Errorf("refresh = %q, want 30s", dashboard.Refresh)
	}
	if dashboard.Time.From != "now-14d" || dashboard.Time.To != "now" {
		t.Errorf("time = %+v, want now-14d..now", dashboard.Time)
	}
	if got, want := strings.Join(dashboard.Tags, ","), "recon,ops"; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
	if len(dashboard.Templating.List) != 0 {
		t.Errorf("templating variables = %d, want none", len(dashboard.Templating.List))
	}
	if len(dashboard.Inputs) != 0 {
		t.Errorf("__inputs = %d, want none", len(dashboard.Inputs))
	}

	type wantPanel struct {
		id        int
		title     string
		panelType string
		dsType    string
		dsUID     string
		sql       string
		expr      string
		format    string
	}
	want := []wantPanel{
		{
			id: 1, title: "Match rate by source", panelType: "timeseries",
			dsType: "grafana-postgresql-datasource", dsUID: postgresDatasourceUID,
			format: "time_series",
			sql: "SELECT bucket_start AS time, source, match_rate\n" +
				"FROM v_ops_match_rate\n" +
				"WHERE $__timeFilter(bucket_start)\n" +
				"ORDER BY bucket_start, source;",
		},
		{
			id: 2, title: "Unmatched value by currency", panelType: "table",
			dsType: "grafana-postgresql-datasource", dsUID: postgresDatasourceUID,
			format: "table",
			sql: "SELECT currency, exposure_major, exposure_minor, signed_delta_minor, break_count\n" +
				"FROM v_ops_unmatched_value\n" +
				"ORDER BY currency;",
		},
		{
			id: 3, title: "Open breaks by type and aging bucket", panelType: "table",
			dsType: "grafana-postgresql-datasource", dsUID: postgresDatasourceUID,
			format: "table",
			sql: "SELECT break_type, bucket, count(*)::bigint AS breaks,\n" +
				"       SUM(abs(delta_minor))::bigint AS exposure_minor\n" +
				"FROM v_ops_break_aging\n" +
				"GROUP BY break_type, bucket, bucket_order\n" +
				"ORDER BY break_type, bucket_order;",
		},
		{
			id: 4, title: "Approval queue", panelType: "table",
			dsType: "grafana-postgresql-datasource", dsUID: postgresDatasourceUID,
			format: "table",
			sql: "SELECT risk_tier,\n" +
				"       count(*) FILTER (WHERE status = 'pending_approval')::bigint AS pending,\n" +
				"       COALESCE(max(pending_seconds) FILTER (WHERE status = 'pending_approval'), 0)::bigint\n" +
				"           AS oldest_pending_seconds,\n" +
				"       count(*) FILTER (WHERE decision = 'approved')::bigint AS approved,\n" +
				"       count(*) FILTER (WHERE decision = 'rejected')::bigint AS rejected,\n" +
				"       ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY approval_seconds)\n" +
				"             FILTER (WHERE decision = 'approved' AND $__timeFilter(approved_at)))::bigint\n" +
				"           AS median_approval_seconds,\n" +
				"       ROUND(percentile_cont(0.95) WITHIN GROUP (ORDER BY approval_seconds)\n" +
				"             FILTER (WHERE decision = 'approved' AND $__timeFilter(approved_at)))::bigint\n" +
				"           AS p95_approval_seconds\n" +
				"FROM v_ops_approval_queue\n" +
				"GROUP BY risk_tier\n" +
				"ORDER BY risk_tier;",
		},
		{
			id: 5, title: "Resolution mix over time", panelType: "timeseries",
			dsType: "grafana-postgresql-datasource", dsUID: postgresDatasourceUID,
			format: "time_series",
			sql: "SELECT (date_trunc('day', event_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AS time,\n" +
				"       classification, count(*)::bigint AS cases\n" +
				"FROM v_ops_resolution_mix\n" +
				"WHERE $__timeFilter(event_at)\n" +
				"GROUP BY 1, 2\n" +
				"ORDER BY 1, 2;",
		},
		{
			id: 6, title: "Prometheus self-scrape", panelType: "stat",
			dsType: "prometheus", dsUID: prometheusDatasourceUID,
			expr: `up{job="prometheus"}`,
		},
	}

	panels := make(map[int]*dashboards.Panel, len(dashboard.Panels))
	for i := range dashboard.Panels {
		panel := &dashboard.Panels[i]
		if _, duplicate := panels[panel.ID]; duplicate {
			t.Fatalf("duplicate panel id %d", panel.ID)
		}
		panels[panel.ID] = panel
	}
	if len(dashboard.Panels) != len(want) || len(panels) != len(want) {
		t.Fatalf("panels = %d, want %d with ids 1-6", len(dashboard.Panels), len(want))
	}

	for _, wantPanel := range want {
		panel, ok := panels[wantPanel.id]
		if !ok {
			t.Errorf("panel id %d is missing", wantPanel.id)
			continue
		}
		if panel.Title != wantPanel.title {
			t.Errorf("panel %d title = %q, want %q", panel.ID, panel.Title, wantPanel.title)
		}
		if panel.Type != wantPanel.panelType {
			t.Errorf("panel %d type = %q, want %q", panel.ID, panel.Type, wantPanel.panelType)
		}
		if panel.Datasource.Type != wantPanel.dsType || panel.Datasource.UID != wantPanel.dsUID {
			t.Errorf("panel %d datasource = %+v, want type=%s uid=%s",
				panel.ID, panel.Datasource, wantPanel.dsType, wantPanel.dsUID)
		}
		if len(panel.Targets) != 1 {
			t.Errorf("panel %d targets = %d, want 1", panel.ID, len(panel.Targets))
			continue
		}
		target := panel.Targets[0]
		if target.RefID == "" {
			t.Errorf("panel %d target has empty refId", panel.ID)
		}
		if wantPanel.sql != "" {
			if got := strings.TrimSpace(target.RawSQL); got != wantPanel.sql {
				t.Errorf("panel %d SQL = %q, want %q", panel.ID, got, wantPanel.sql)
			}
			if target.Format != wantPanel.format {
				t.Errorf("panel %d format = %q, want %q", panel.ID, target.Format, wantPanel.format)
			}
		}
		if wantPanel.expr != "" {
			if got := strings.TrimSpace(target.Expr); got != wantPanel.expr {
				t.Errorf("panel %d expr = %q, want %q", panel.ID, got, wantPanel.expr)
			}
		}
		if panel.GridPosition.W == 0 || panel.GridPosition.H == 0 {
			t.Errorf("panel %d has empty grid position: %+v", panel.ID, panel.GridPosition)
		}
	}

	assertPanelUnits(t, panels)
	assertPanelQueriesUseFrozenViews(t, panels)
	assertCanonicalAgingLabels(t, panels[3])
	assertStackedBars(t, panels[5])
	assertPrometheusStat(t, panels[6])
	assertNoDashboardSecrets(t, readRepoFile(t, "deploy/grafana/dashboards/ops.json"))
}

func assertPanelUnits(t *testing.T, panels map[int]*dashboards.Panel) {
	t.Helper()
	unit := func(panel *dashboards.Panel) string {
		var config struct {
			Defaults struct {
				Unit string `json:"unit"`
			} `json:"defaults"`
		}
		if panel == nil || len(panel.FieldConfig) == 0 {
			return ""
		}
		if err := json.Unmarshal(panel.FieldConfig, &config); err != nil {
			t.Fatalf("parse panel %d fieldConfig: %v", panel.ID, err)
		}
		return config.Defaults.Unit
	}
	if got := unit(panels[1]); got != "percentunit" {
		t.Errorf("panel 1 unit = %q, want percentunit", got)
	}
	if got := unit(panels[4]); got != "s" {
		t.Errorf("panel 4 default unit = %q, want s", got)
	}
	if got := unit(panels[5]); got != "short" {
		t.Errorf("panel 5 unit = %q, want short (cases/day)", got)
	}
	if got := unit(panels[6]); got != "short" {
		t.Errorf("panel 6 unit = %q, want short", got)
	}

	if !strings.Contains(strings.ToLower(panels[2].Description), "exposure proxy") ||
		!strings.Contains(panels[2].Description, "No cross-currency total") {
		t.Errorf("panel 2 description does not state the exposure-proxy boundary: %q", panels[2].Description)
	}
	if !strings.Contains(panels[4].Description, "snapshot") ||
		!strings.Contains(panels[4].Description, "rejected") {
		t.Errorf("panel 4 description does not state the pending snapshot/fail-closed rules: %q",
			panels[4].Description)
	}

	var panel2Options struct {
		Footer struct {
			Show bool `json:"show"`
		} `json:"footer"`
	}
	if err := json.Unmarshal(panels[2].Options, &panel2Options); err != nil {
		t.Fatalf("parse panel 2 options: %v", err)
	}
	if panel2Options.Footer.Show {
		t.Error("panel 2 table footer (total row) is enabled; want no total row")
	}

	var panel4Config struct {
		Overrides []struct {
			Matcher struct {
				Options string `json:"options"`
			} `json:"matcher"`
			Properties []struct {
				ID    string `json:"id"`
				Value any    `json:"value"`
			} `json:"properties"`
		} `json:"overrides"`
	}
	if err := json.Unmarshal(panels[4].FieldConfig, &panel4Config); err != nil {
		t.Fatalf("parse panel 4 fieldConfig: %v", err)
	}
	seconds := map[string]bool{
		"oldest_pending_seconds":  false,
		"median_approval_seconds": false,
		"p95_approval_seconds":    false,
	}
	counts := map[string]bool{
		"pending":  false,
		"approved": false,
		"rejected": false,
	}
	for _, override := range panel4Config.Overrides {
		for _, property := range override.Properties {
			if property.ID != "unit" {
				continue
			}
			if property.Value == "s" {
				if _, ok := seconds[override.Matcher.Options]; ok {
					seconds[override.Matcher.Options] = true
				}
			}
			if property.Value == "short" {
				if _, ok := counts[override.Matcher.Options]; ok {
					counts[override.Matcher.Options] = true
				}
			}
		}
	}
	for column, ok := range seconds {
		if !ok {
			t.Errorf("panel 4 column %s is not configured in seconds", column)
		}
	}
	for column, ok := range counts {
		if !ok {
			t.Errorf("panel 4 count column %s is not configured as short", column)
		}
	}
}

func assertPanelQueriesUseFrozenViews(t *testing.T, panels map[int]*dashboards.Panel) {
	t.Helper()
	wantViews := map[int]string{
		1: "v_ops_match_rate",
		2: "v_ops_unmatched_value",
		3: "v_ops_break_aging",
		4: "v_ops_approval_queue",
		5: "v_ops_resolution_mix",
	}
	allowedMacros := []string{
		"$__timeFilter(bucket_start)",
		"$__timeFilter(event_at)",
		"$__timeFilter(approved_at)",
	}
	for id, view := range wantViews {
		panel := panels[id]
		if panel == nil || len(panel.Targets) == 0 {
			t.Errorf("panel %d is missing a target", id)
			continue
		}
		sql := panel.Targets[0].RawSQL
		if !strings.Contains(sql, "FROM "+view) {
			t.Errorf("panel %d SQL does not read %s: %q", id, view, sql)
		}
		remaining := sql
		for _, macro := range allowedMacros {
			remaining = strings.ReplaceAll(remaining, macro, "")
		}
		if strings.Contains(remaining, "$__") {
			t.Errorf("panel %d SQL uses a disallowed macro: %q", id, sql)
		}
		if strings.Contains(sql, "${") {
			t.Errorf("panel %d SQL uses a non-time macro: %q", id, sql)
		}
	}

	if got := strings.Count(panels[4].Targets[0].RawSQL, "$__timeFilter(approved_at)"); got != 2 {
		t.Errorf("panel 4 approved_at time filters = %d, want 2", got)
	}
}

func assertCanonicalAgingLabels(t *testing.T, panel *dashboards.Panel) {
	t.Helper()
	if panel == nil {
		t.Fatal("panel 3 is missing")
	}
	last := -1
	for _, label := range []string{"0-1", "2-3", "4-7", ">7", "unknown"} {
		index := strings.Index(panel.Description, label)
		if index < 0 {
			t.Errorf("panel 3 description is missing canonical bucket %q", label)
			continue
		}
		if index <= last {
			t.Errorf("panel 3 bucket %q is out of canonical order in %q", label, panel.Description)
		}
		last = index
	}
}

func assertStackedBars(t *testing.T, panel *dashboards.Panel) {
	t.Helper()
	if panel == nil {
		t.Fatal("panel 5 is missing")
	}
	var config struct {
		Defaults struct {
			Custom struct {
				DrawStyle string `json:"drawStyle"`
				Stacking  struct {
					Mode string `json:"mode"`
				} `json:"stacking"`
			} `json:"custom"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(panel.FieldConfig, &config); err != nil {
		t.Fatalf("parse panel 5 fieldConfig: %v", err)
	}
	if config.Defaults.Custom.DrawStyle != "bars" || config.Defaults.Custom.Stacking.Mode != "normal" {
		t.Errorf("panel 5 display = drawStyle %q, stacking %q; want bars/normal",
			config.Defaults.Custom.DrawStyle, config.Defaults.Custom.Stacking.Mode)
	}
}

func assertPrometheusStat(t *testing.T, panel *dashboards.Panel) {
	t.Helper()
	if panel == nil {
		t.Fatal("panel 6 is missing")
	}
	if panel.Title != "Prometheus self-scrape" || panel.Type != "stat" {
		t.Errorf("panel 6 = %q/%q, want Prometheus self-scrape/stat", panel.Title, panel.Type)
	}
	if len(panel.Targets) != 1 || panel.Targets[0].Expr != `up{job="prometheus"}` {
		t.Errorf("panel 6 target = %+v, want up{job=\"prometheus\"}", panel.Targets)
	}
	var config struct {
		Defaults struct {
			Thresholds struct {
				Steps []struct {
					Color string `json:"color"`
					Value any    `json:"value"`
				} `json:"steps"`
			} `json:"thresholds"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(panel.FieldConfig, &config); err != nil {
		t.Fatalf("parse panel 6 fieldConfig: %v", err)
	}
	steps := config.Defaults.Thresholds.Steps
	if len(steps) < 2 {
		t.Fatalf("panel 6 thresholds = %+v, want at least red@null and green@1", steps)
	}
	last := steps[len(steps)-1]
	if last.Color != "green" || last.Value != float64(1) {
		t.Errorf("panel 6 final threshold = %+v, want green@1", last)
	}
}

func assertNoDashboardSecrets(t *testing.T, raw []byte) {
	t.Helper()
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse dashboard for secret scan: %v", err)
	}
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				switch {
				case strings.Contains(lower, "password"),
					strings.Contains(lower, "secret"),
					strings.Contains(lower, "token"),
					strings.Contains(lower, "api_key"),
					strings.Contains(lower, "apikey"):
					t.Errorf("dashboard contains credential-like key %s", path+"."+key)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for _, child := range typed {
				walk(path, child)
			}
		case string:
			for _, forbidden := range []string{
				"postgres://", "postgresql://",
				"recon_dev_dashboards_ro_local_only", "recon_admin_local_only",
			} {
				if strings.Contains(typed, forbidden) {
					t.Errorf("dashboard contains literal credential %q at %s", forbidden, path)
				}
			}
		}
	}
	walk("$", document)
}

// TestM504Provisioning pins datasource/provider YAML, the Prometheus
// self-scrape config, and the compose service/security shape.
func TestM504Provisioning(t *testing.T) {
	t.Run("datasources", func(t *testing.T) {
		postgres := loadYAML[datasourceDocument](t, "deploy/grafana/provisioning/datasources/postgres.yaml")
		if postgres.APIVersion != 1 {
			t.Errorf("postgres apiVersion = %d, want 1", postgres.APIVersion)
		}
		if len(postgres.Datasources) != 1 {
			t.Fatalf("postgres datasources = %d, want 1", len(postgres.Datasources))
		}
		ds := postgres.Datasources[0]
		if ds.UID != postgresDatasourceUID || ds.Type != "grafana-postgresql-datasource" {
			t.Errorf("postgres identity = %s/%s, want %s/grafana-postgresql-datasource",
				ds.UID, ds.Type, postgresDatasourceUID)
		}
		if ds.URL != "postgres:5432" || ds.Database != "recon" ||
			ds.User != "${RECON_DASHBOARDS_RO_USER}" {
			t.Errorf("postgres connection = url %q database %q user %q",
				ds.URL, ds.Database, ds.User)
		}
		if !ds.IsDefault || ds.Editable {
			t.Errorf("postgres flags = isDefault %t editable %t, want true/false", ds.IsDefault, ds.Editable)
		}
		for key, want := range map[string]any{
			"sslmode":          "disable",
			"postgresVersion":  1600,
			"timescaledb":      false,
			"maxOpenConns":     4,
			"maxIdleConns":     2,
			"timezone":         "UTC",
			"session_timezone": "UTC",
		} {
			if got := ds.JSONData[key]; got != want {
				t.Errorf("postgres jsonData.%s = %#v, want %#v", key, got, want)
			}
		}
		if len(ds.SecureJSONData) != 1 ||
			ds.SecureJSONData["password"] != "${RECON_DASHBOARDS_RO_PASSWORD}" {
			t.Errorf("postgres secureJsonData = %#v, want only password=${RECON_DASHBOARDS_RO_PASSWORD}",
				ds.SecureJSONData)
		}

		prom := loadYAML[datasourceDocument](t, "deploy/grafana/provisioning/datasources/prometheus.yaml")
		if prom.APIVersion != 1 || len(prom.Datasources) != 1 {
			t.Fatalf("prometheus datasource document = apiVersion %d, %d datasources", prom.APIVersion, len(prom.Datasources))
		}
		pds := prom.Datasources[0]
		if pds.UID != prometheusDatasourceUID || pds.Type != "prometheus" || pds.URL != "http://prometheus:9090" {
			t.Errorf("prometheus identity/url = %s/%s/%s", pds.UID, pds.Type, pds.URL)
		}
		if pds.IsDefault || pds.Editable {
			t.Errorf("prometheus flags = isDefault %t editable %t, want false/false", pds.IsDefault, pds.Editable)
		}
		if pds.JSONData["httpMethod"] != "POST" || pds.JSONData["timeInterval"] != "15s" {
			t.Errorf("prometheus jsonData = %#v", pds.JSONData)
		}

		for _, rel := range []string{
			"deploy/grafana/provisioning/datasources/postgres.yaml",
			"deploy/grafana/provisioning/datasources/prometheus.yaml",
		} {
			raw := string(readRepoFile(t, rel))
			for _, forbidden := range []string{
				"recon_dev_dashboards_ro_local_only", "recon_admin_local_only", "postgres://",
			} {
				if strings.Contains(raw, forbidden) {
					t.Errorf("%s contains literal credential %q", rel, forbidden)
				}
			}
		}
	})

	t.Run("provider", func(t *testing.T) {
		doc := loadYAML[providerDocument](t, "deploy/grafana/provisioning/dashboards/provider.yaml")
		if doc.APIVersion != 1 || len(doc.Providers) != 1 {
			t.Fatalf("provider document = apiVersion %d, %d providers", doc.APIVersion, len(doc.Providers))
		}
		p := doc.Providers[0]
		if p.Name != "recon" || p.OrgID != 1 || p.Folder != "Recon" || p.Type != "file" {
			t.Errorf("provider identity = %+v", p)
		}
		if !p.DisableDeletion || p.AllowUIUpdates {
			t.Errorf("provider flags = disableDeletion %t allowUiUpdates %t, want true/false",
				p.DisableDeletion, p.AllowUIUpdates)
		}
		if p.UpdateIntervalSeconds != 30 {
			t.Errorf("provider updateIntervalSeconds = %d, want 30", p.UpdateIntervalSeconds)
		}
		if p.Options.Path != "/var/lib/grafana/dashboards" || p.Options.FoldersFromFilesStructure {
			t.Errorf("provider options = %+v", p.Options)
		}
	})

	t.Run("prometheus config", func(t *testing.T) {
		doc := loadYAML[prometheusDocument](t, "deploy/prometheus/prometheus.yml")
		if doc.Global.ScrapeInterval != "15s" {
			t.Errorf("scrape_interval = %q, want 15s", doc.Global.ScrapeInterval)
		}
		if len(doc.ScrapeConfigs) != 1 {
			t.Fatalf("scrape_configs = %d, want only the self-scrape job", len(doc.ScrapeConfigs))
		}
		job := doc.ScrapeConfigs[0]
		if job.JobName != "prometheus" {
			t.Errorf("job_name = %q, want prometheus", job.JobName)
		}
		if len(job.StaticConfigs) != 1 || len(job.StaticConfigs[0].Targets) != 1 ||
			job.StaticConfigs[0].Targets[0] != "localhost:9090" {
			t.Errorf("prometheus static targets = %+v, want only localhost:9090", job.StaticConfigs)
		}
	})

	t.Run("compose", func(t *testing.T) {
		doc := loadYAML[composeDocument](t, "compose.yaml")
		for _, forbidden := range []string{"api", "worker", "outbox", "recon-api", "recon-worker"} {
			if _, ok := doc.Services[forbidden]; ok {
				t.Errorf("compose unexpectedly adds app service %q", forbidden)
			}
		}

		prom, ok := doc.Services["prometheus"]
		if !ok {
			t.Fatal("compose is missing the prometheus service")
		}
		if prom.Image != "prom/prometheus:v3.16.0" {
			t.Errorf("prometheus image = %q, want pinned prom/prometheus:v3.16.0", prom.Image)
		}
		requireLoopbackPort(t, "prometheus", prom.Ports)
		requireMount(t, "prometheus", prom.Volumes, "./deploy/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro")
		requireMount(t, "prometheus", prom.Volumes, "recon-prometheus-data:/prometheus")
		requireHealthcheck(t, "prometheus", prom.Healthcheck, "/-/ready")

		grafana, ok := doc.Services["grafana"]
		if !ok {
			t.Fatal("compose is missing the grafana service")
		}
		if grafana.Image != "grafana/grafana:13.3.2" {
			t.Errorf("grafana image = %q, want pinned grafana/grafana:13.3.2", grafana.Image)
		}
		requireLoopbackPort(t, "grafana", grafana.Ports)
		requireMount(t, "grafana", grafana.Volumes, "./deploy/grafana/provisioning:/etc/grafana/provisioning:ro")
		requireMount(t, "grafana", grafana.Volumes, "./deploy/grafana/dashboards:/var/lib/grafana/dashboards:ro")
		requireMount(t, "grafana", grafana.Volumes, "recon-grafana-data:/var/lib/grafana")
		requireHealthcheck(t, "grafana", grafana.Healthcheck, "/api/health")
		for _, volume := range grafana.Volumes {
			if strings.Contains(volume, "docker.sock") {
				t.Errorf("grafana mounts the Docker socket: %q", volume)
			}
		}

		wantEnv := map[string]string{
			"GF_SECURITY_ADMIN_USER":                "${RECON_GRAFANA_ADMIN_USER:-admin}",
			"GF_SECURITY_ADMIN_PASSWORD":            "${RECON_GRAFANA_ADMIN_PASSWORD:-recon_admin_local_only}",
			"GF_AUTH_ANONYMOUS_ENABLED":             "false",
			"GF_USERS_ALLOW_SIGN_UP":                "false",
			"GF_ANALYTICS_REPORTING_ENABLED":        "false",
			"GF_ANALYTICS_CHECK_FOR_UPDATES":        "false",
			"GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES": "false",
			"GF_PLUGINS_PREINSTALL_DISABLED":        "true",
			"GF_INSTALL_PLUGINS":                    "",
			"RECON_DASHBOARDS_RO_USER":              "${RECON_DASHBOARDS_RO_USER:-recon_dev_dashboards_ro}",
			"RECON_DASHBOARDS_RO_PASSWORD":          "${RECON_DASHBOARDS_RO_PASSWORD:-recon_dev_dashboards_ro_local_only}",
		}
		for key, want := range wantEnv {
			if got := grafana.Environment[key]; got != want {
				t.Errorf("grafana environment %s = %q, want %q", key, got, want)
			}
		}
		if got := grafana.DependsOn["postgres"].Condition; got != "service_healthy" {
			t.Errorf("grafana depends_on postgres = %q, want service_healthy", got)
		}
		if got := grafana.DependsOn["prometheus"].Condition; got != "service_started" {
			t.Errorf("grafana depends_on prometheus = %q, want service_started", got)
		}

		for _, volume := range []string{"recon-prometheus-data", "recon-grafana-data"} {
			if _, ok := doc.Volumes[volume]; !ok {
				t.Errorf("compose named volume %q is not declared", volume)
			}
		}
	})
}

func requireLoopbackPort(t *testing.T, service string, ports []string) {
	t.Helper()
	for _, port := range ports {
		if strings.HasPrefix(port, "127.0.0.1:") {
			return
		}
	}
	t.Errorf("%s ports = %v, want a 127.0.0.1-bound published port", service, ports)
}

func requireMount(t *testing.T, service string, mounts []string, want string) {
	t.Helper()
	for _, mount := range mounts {
		if mount == want {
			return
		}
	}
	t.Errorf("%s mounts = %v, want %q", service, mounts, want)
}

func requireHealthcheck(t *testing.T, service string, healthcheck *composeHealthcheck, probe string) {
	t.Helper()
	if healthcheck == nil {
		t.Errorf("%s has no healthcheck", service)
		return
	}
	if len(healthcheck.Test) == 0 || !strings.Contains(strings.Join(healthcheck.Test, " "), probe) {
		t.Errorf("%s healthcheck = %v, want a probe of %s", service, healthcheck.Test, probe)
	}
	if healthcheck.Interval == "" || healthcheck.Timeout == "" || healthcheck.Retries == 0 {
		t.Errorf("%s healthcheck is incomplete: %+v", service, healthcheck)
	}
}
