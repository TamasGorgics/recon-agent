package dashboards

import (
	"encoding/json"
	"fmt"
	"os"
)

const (
	// PostgresDatasourceUID is the stable provisioning UID of the dashboard's
	// read-only Postgres datasource.
	PostgresDatasourceUID = "recon-postgres"
	// PrometheusDatasourceUID is the stable provisioning UID of the dashboard's
	// supplementary Prometheus datasource.
	PrometheusDatasourceUID = "recon-prometheus"
	// DashboardUID is the stable identity used by the provisioning file and the
	// live dashboard checks.
	DashboardUID = "recon-ops"
)

// Dashboard is the provisioned Grafana dashboard document. Only the fields the
// M5-04 contract and tests need are typed; the rest of the document is left to
// Grafana.
type Dashboard struct {
	UID        string            `json:"uid"`
	Title      string            `json:"title"`
	Timezone   string            `json:"timezone"`
	Refresh    string            `json:"refresh"`
	Tags       []string          `json:"tags"`
	Time       DashboardTime     `json:"time"`
	Templating Templating        `json:"templating"`
	Inputs     []json.RawMessage `json:"__inputs"`
	Panels     []Panel           `json:"panels"`
}

// DashboardTime is the dashboard's default time range.
type DashboardTime struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Templating is the dashboard variable list. M5-04 deliberately has none.
type Templating struct {
	List []json.RawMessage `json:"list"`
}

// Panel is a dashboard panel. The typed fields are the frozen M5-04 contract;
// fieldConfig and options stay raw because they are display-only.
type Panel struct {
	ID           int             `json:"id"`
	Title        string          `json:"title"`
	Type         string          `json:"type"`
	Description  string          `json:"description"`
	Datasource   DatasourceRef   `json:"datasource"`
	Targets      []Target        `json:"targets"`
	FieldConfig  json.RawMessage `json:"fieldConfig"`
	Options      json.RawMessage `json:"options"`
	GridPosition GridPosition    `json:"gridPos"`
}

// DatasourceRef identifies a provisioned datasource by its stable UID.
type DatasourceRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// Target is one panel query. Postgres targets use RawSQL and Prometheus
// targets use Expr.
type Target struct {
	RefID      string `json:"refId"`
	RawSQL     string `json:"rawSql"`
	Expr       string `json:"expr"`
	Format     string `json:"format"`
	EditorMode string `json:"editorMode"`
}

// GridPosition is the deterministic panel layout position.
type GridPosition struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

// LoadDashboardFile reads and parses a provisioned dashboard JSON file.
func LoadDashboardFile(path string) (*Dashboard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dashboard file: %w", err)
	}
	var dashboard Dashboard
	if err := json.Unmarshal(data, &dashboard); err != nil {
		return nil, fmt.Errorf("parse dashboard file: %w", err)
	}
	return &dashboard, nil
}

// PanelByID returns the panel with the given stable ID, or nil.
func (d *Dashboard) PanelByID(id int) *Panel {
	for i := range d.Panels {
		if d.Panels[i].ID == id {
			return &d.Panels[i]
		}
	}
	return nil
}
