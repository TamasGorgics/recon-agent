package llm

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

//go:embed pricing.yaml
var pricingYAML []byte

const (
	pricesPerMillion = 1_000_000
	maxFallbackChain = 3
)

type pricingDocument struct {
	SchemaVersion int             `yaml:"schema_version" json:"schema_version"`
	SourceURL     string          `yaml:"source_url" json:"source_url"`
	FetchedAt     string          `yaml:"fetched_at" json:"fetched_at"`
	Models        []modelDocument `yaml:"models" json:"models"`
}

type modelDocument struct {
	Provider        string               `yaml:"provider" json:"provider"`
	ID              string               `yaml:"id" json:"id"`
	Selector        string               `yaml:"selector" json:"selector"`
	ContextLimit    int                  `yaml:"context_limit" json:"context_limit"`
	OutputLimit     int                  `yaml:"output_limit" json:"output_limit"`
	Capabilities    capabilitiesDocument `yaml:"capabilities" json:"capabilities"`
	Prices          pricesDocument       `yaml:"prices_per_million" json:"prices_per_million"`
	Peak            *peakDocument        `yaml:"peak" json:"peak"`
	AllowedFallback bool                 `yaml:"allowed_fallback" json:"allowed_fallback"`
}

type capabilitiesDocument struct {
	Tools            bool     `yaml:"tools" json:"tools"`
	ToolChoice       []string `yaml:"tool_choice" json:"tool_choice"`
	Temperature      bool     `yaml:"temperature" json:"temperature"`
	Reasoning        bool     `yaml:"reasoning" json:"reasoning"`
	StructuredOutput bool     `yaml:"structured_output" json:"structured_output"`
	Cache            bool     `yaml:"cache" json:"cache"`
}

type pricesDocument struct {
	Input        string `yaml:"input" json:"input"`
	Output       string `yaml:"output" json:"output"`
	CacheRead    string `yaml:"cache_read" json:"cache_read"`
	CacheWrite5m string `yaml:"cache_write_5m" json:"cache_write_5m"`
	CacheWrite1h string `yaml:"cache_write_1h" json:"cache_write_1h"`
}

type peakDocument struct {
	Multiplier string           `yaml:"multiplier" json:"multiplier"`
	Windows    []windowDocument `yaml:"windows" json:"windows"`
}

type windowDocument struct {
	Days     []string `yaml:"days" json:"days"`
	Start    string   `yaml:"start" json:"start"`
	End      string   `yaml:"end" json:"end"`
	Timezone string   `yaml:"timezone" json:"timezone"`
}

// Capabilities describes what a model supports.
type Capabilities struct {
	Tools            bool
	ToolChoice       []string
	Temperature      bool
	Reasoning        bool
	StructuredOutput bool
	Cache            bool
}

// Prices holds per-million-token prices as exact decimals.
type Prices struct {
	Input        decimal.Decimal
	Output       decimal.Decimal
	CacheRead    decimal.Decimal
	CacheWrite5m decimal.Decimal
	CacheWrite1h decimal.Decimal
}

// PeakWindow is a half-open [StartMinute, EndMinute) UTC weekday window.
type PeakWindow struct {
	Days        []time.Weekday
	StartMinute int
	EndMinute   int
}

// Peak describes the peak multiplier and its UTC windows.
type Peak struct {
	Multiplier decimal.Decimal
	Windows    []PeakWindow
}

// Model is one validated pricing record.
type Model struct {
	Provider        string
	ID              string
	Selector        string
	ContextLimit    int
	OutputLimit     int
	Capabilities    Capabilities
	Prices          Prices
	Peak            *Peak
	AllowedFallback bool
}

// Registry is the immutable embedded pricing registry.
type Registry struct {
	SchemaVersion int
	SourceURL     string
	FetchedAt     string
	models        map[string]*Model
}

var defaultRegistry = mustLoadDefaultRegistry()

func mustLoadDefaultRegistry() *Registry {
	registry, err := LoadRegistry(pricingYAML)
	if err != nil {
		panic(fmt.Sprintf("llm: invalid embedded pricing.yaml: %v", err))
	}
	return registry
}

// DefaultRegistry returns the embedded, validated pricing registry.
func DefaultRegistry() *Registry { return defaultRegistry }

// LoadRegistry parses and validates a pricing document.
func LoadRegistry(data []byte) (*Registry, error) {
	var doc pricingDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse pricing: %w", err)
	}
	if err := validatePricingDocument(&doc); err != nil {
		return nil, err
	}
	registry := &Registry{
		SchemaVersion: doc.SchemaVersion,
		SourceURL:     doc.SourceURL,
		FetchedAt:     doc.FetchedAt,
		models:        make(map[string]*Model, len(doc.Models)),
	}
	for _, entry := range doc.Models {
		model, err := buildModel(entry)
		if err != nil {
			return nil, err
		}
		registry.models[model.ID] = model
	}
	return registry, nil
}

func validatePricingDocument(doc *pricingDocument) error {
	if doc.SchemaVersion != 1 {
		return fmt.Errorf("pricing: unsupported schema_version %d", doc.SchemaVersion)
	}
	if strings.TrimSpace(doc.SourceURL) == "" {
		return errors.New("pricing: source_url is required")
	}
	parsed, err := url.Parse(doc.SourceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("pricing: source_url %q must be an absolute https URL", doc.SourceURL)
	}
	if _, err := time.Parse("2006-01-02", doc.FetchedAt); err != nil {
		return fmt.Errorf("pricing: fetched_at %q must be YYYY-MM-DD", doc.FetchedAt)
	}
	if len(doc.Models) == 0 {
		return errors.New("pricing: models is empty")
	}
	seen := make(map[string]struct{}, len(doc.Models))
	for i := range doc.Models {
		entry := &doc.Models[i]
		if err := ValidateModelID(entry.ID); err != nil {
			return fmt.Errorf("pricing: model %d: %w", i, err)
		}
		if entry.ID != APIID(entry.ID) {
			return fmt.Errorf("pricing: model id %q must be bare, not a selector", entry.ID)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("pricing: duplicate model %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		if entry.Provider != ProviderOpenCodeGo {
			return fmt.Errorf("pricing: model %q provider %q must be %q", entry.ID, entry.Provider, ProviderOpenCodeGo)
		}
		if entry.Selector != Selector(entry.ID) {
			return fmt.Errorf("pricing: model %q selector %q must be %q", entry.ID, entry.Selector, Selector(entry.ID))
		}
		if entry.ContextLimit <= 0 || entry.OutputLimit <= 0 {
			return fmt.Errorf("pricing: model %q limits must be positive", entry.ID)
		}
		if entry.OutputLimit > entry.ContextLimit {
			return fmt.Errorf("pricing: model %q output_limit exceeds context_limit", entry.ID)
		}
		for _, mode := range entry.Capabilities.ToolChoice {
			switch ToolChoiceMode(mode) {
			case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired, ToolChoiceTool:
			default:
				return fmt.Errorf("pricing: model %q has invalid tool_choice %q", entry.ID, mode)
			}
		}
		for name, raw := range map[string]string{
			"input":          entry.Prices.Input,
			"output":         entry.Prices.Output,
			"cache_read":     entry.Prices.CacheRead,
			"cache_write_5m": entry.Prices.CacheWrite5m,
			"cache_write_1h": entry.Prices.CacheWrite1h,
		} {
			price, err := decimal.NewFromString(raw)
			if err != nil {
				return fmt.Errorf("pricing: model %q %s price %q is not a decimal", entry.ID, name, raw)
			}
			if price.IsNegative() {
				return fmt.Errorf("pricing: model %q %s price is negative", entry.ID, name)
			}
		}
		if entry.Peak != nil {
			if err := validatePeak(entry.ID, entry.Peak); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePeak(modelID string, peak *peakDocument) error {
	multiplier, err := decimal.NewFromString(peak.Multiplier)
	if err != nil || !multiplier.GreaterThan(decimal.Zero) {
		return fmt.Errorf("pricing: model %q peak multiplier %q must be a positive decimal", modelID, peak.Multiplier)
	}
	if len(peak.Windows) == 0 {
		return fmt.Errorf("pricing: model %q peak has no windows", modelID)
	}
	for i, window := range peak.Windows {
		if len(window.Days) == 0 {
			return fmt.Errorf("pricing: model %q peak window %d has no days", modelID, i)
		}
		for _, day := range window.Days {
			if _, err := parseWeekday(day); err != nil {
				return fmt.Errorf("pricing: model %q peak window %d: %w", modelID, i, err)
			}
		}
		start, err := parseClock(window.Start)
		if err != nil {
			return fmt.Errorf("pricing: model %q peak window %d start: %w", modelID, i, err)
		}
		end, err := parseClock(window.End)
		if err != nil {
			return fmt.Errorf("pricing: model %q peak window %d end: %w", modelID, i, err)
		}
		if start >= end {
			return fmt.Errorf("pricing: model %q peak window %d must have start < end", modelID, i)
		}
		if !strings.EqualFold(window.Timezone, "UTC") {
			return fmt.Errorf("pricing: model %q peak window %d timezone must be UTC", modelID, i)
		}
	}
	return nil
}

func buildModel(entry modelDocument) (*Model, error) {
	price := func(name, raw string) (decimal.Decimal, error) {
		d, err := decimal.NewFromString(raw)
		if err != nil {
			return decimal.Zero, fmt.Errorf("pricing: model %q %s price: %w", entry.ID, name, err)
		}
		return d, nil
	}
	input, err := price("input", entry.Prices.Input)
	if err != nil {
		return nil, err
	}
	output, err := price("output", entry.Prices.Output)
	if err != nil {
		return nil, err
	}
	cacheRead, err := price("cache_read", entry.Prices.CacheRead)
	if err != nil {
		return nil, err
	}
	cacheWrite5m, err := price("cache_write_5m", entry.Prices.CacheWrite5m)
	if err != nil {
		return nil, err
	}
	cacheWrite1h, err := price("cache_write_1h", entry.Prices.CacheWrite1h)
	if err != nil {
		return nil, err
	}
	model := &Model{
		Provider:     entry.Provider,
		ID:           entry.ID,
		Selector:     entry.Selector,
		ContextLimit: entry.ContextLimit,
		OutputLimit:  entry.OutputLimit,
		Capabilities: Capabilities{
			Tools:            entry.Capabilities.Tools,
			ToolChoice:       append([]string(nil), entry.Capabilities.ToolChoice...),
			Temperature:      entry.Capabilities.Temperature,
			Reasoning:        entry.Capabilities.Reasoning,
			StructuredOutput: entry.Capabilities.StructuredOutput,
			Cache:            entry.Capabilities.Cache,
		},
		Prices: Prices{
			Input:        input,
			Output:       output,
			CacheRead:    cacheRead,
			CacheWrite5m: cacheWrite5m,
			CacheWrite1h: cacheWrite1h,
		},
		AllowedFallback: entry.AllowedFallback,
	}
	if entry.Peak != nil {
		multiplier, _ := decimal.NewFromString(entry.Peak.Multiplier)
		peak := &Peak{Multiplier: multiplier}
		for _, window := range entry.Peak.Windows {
			start, _ := parseClock(window.Start)
			end, _ := parseClock(window.End)
			peakWindow := PeakWindow{StartMinute: start, EndMinute: end}
			for _, day := range window.Days {
				weekday, _ := parseWeekday(day)
				peakWindow.Days = append(peakWindow.Days, weekday)
			}
			peak.Windows = append(peak.Windows, peakWindow)
		}
		model.Peak = peak
	}
	return model, nil
}

func parseWeekday(day string) (time.Weekday, error) {
	switch strings.ToLower(strings.TrimSpace(day)) {
	case "sun":
		return time.Sunday, nil
	case "mon":
		return time.Monday, nil
	case "tue":
		return time.Tuesday, nil
	case "wed":
		return time.Wednesday, nil
	case "thu":
		return time.Thursday, nil
	case "fri":
		return time.Friday, nil
	case "sat":
		return time.Saturday, nil
	default:
		return 0, fmt.Errorf("invalid weekday %q", day)
	}
}

func parseClock(raw string) (int, error) {
	var hour, minute int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%02d:%02d", &hour, &minute); err != nil {
		return 0, fmt.Errorf("invalid clock %q", raw)
	}
	if hour < 0 || hour > 24 || minute < 0 || minute > 59 || (hour == 24 && minute != 0) {
		return 0, fmt.Errorf("invalid clock %q", raw)
	}
	return hour*60 + minute, nil
}

// Get returns a model by API ID or selector. Unknown models return
// ErrUnknownModel.
func (r *Registry) Get(model string) (*Model, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModel, Redact(APIID(model)))
	}
	id := APIID(strings.TrimSpace(model))
	entry, ok := r.models[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModel, Redact(id))
	}
	return entry, nil
}

// Models returns the registered models in a stable order-independent map.
func (r *Registry) Models() map[string]*Model {
	out := make(map[string]*Model, len(r.models))
	for id, model := range r.models {
		out[id] = model
	}
	return out
}

// Tokens is a token breakdown for cost calculation.
type Tokens struct {
	Input        int
	Output       int
	CacheRead    int
	CacheWrite5m int
	CacheWrite1h int
}

// PeakMultiplierAt returns the multiplier that applies at at (UTC). It returns
// 1 when the model has no peak or at falls outside every window.
func (m *Model) PeakMultiplierAt(at time.Time) decimal.Decimal {
	one := decimal.NewFromInt(1)
	if m == nil || m.Peak == nil {
		return one
	}
	utc := at.UTC()
	minute := utc.Hour()*60 + utc.Minute()
	for _, window := range m.Peak.Windows {
		if !containsWeekday(window.Days, utc.Weekday()) {
			continue
		}
		if minute >= window.StartMinute && minute < window.EndMinute {
			return m.Peak.Multiplier
		}
	}
	return one
}

func containsWeekday(days []time.Weekday, day time.Weekday) bool {
	for _, candidate := range days {
		if candidate == day {
			return true
		}
	}
	return false
}

// ComputeCost returns tokens * price / 1,000,000 summed across categories and
// multiplied by any active peak multiplier. All arithmetic is decimal.
func (r *Registry) ComputeCost(model string, tokens Tokens, at time.Time) (decimal.Decimal, error) {
	entry, err := r.Get(model)
	if err != nil {
		return decimal.Zero, err
	}
	return entry.ComputeCost(tokens, at), nil
}

// ComputeCost returns the exact decimal cost for tokens at at.
func (m *Model) ComputeCost(tokens Tokens, at time.Time) decimal.Decimal {
	million := decimal.NewFromInt(pricesPerMillion)
	line := func(count int, price decimal.Decimal) decimal.Decimal {
		return price.Mul(decimal.NewFromInt(int64(count))).Div(million)
	}
	total := line(tokens.Input, m.Prices.Input).
		Add(line(tokens.Output, m.Prices.Output)).
		Add(line(tokens.CacheRead, m.Prices.CacheRead)).
		Add(line(tokens.CacheWrite5m, m.Prices.CacheWrite5m)).
		Add(line(tokens.CacheWrite1h, m.Prices.CacheWrite1h))
	return total.Mul(m.PeakMultiplierAt(at))
}

// AllowedFallback reports whether model exists and is marked allowed_fallback.
func (r *Registry) AllowedFallback(model string) bool {
	entry, err := r.Get(model)
	return err == nil && entry.AllowedFallback
}

// ValidateFallback enforces existence, allowlisting, uniqueness, a different
// primary, and the maximum chain length.
func (r *Registry) ValidateFallback(primary string, fallbacks []string) error {
	primaryID := APIID(strings.TrimSpace(primary))
	if _, err := r.Get(primaryID); err != nil {
		return fmt.Errorf("llm: fallback primary: %w", err)
	}
	if len(fallbacks) > maxFallbackChain {
		return fmt.Errorf("llm: fallback chain has %d models, maximum is %d", len(fallbacks), maxFallbackChain)
	}
	seen := make(map[string]struct{}, len(fallbacks))
	for _, fallback := range fallbacks {
		id := APIID(strings.TrimSpace(fallback))
		if id == "" {
			return errors.New("llm: fallback model is empty")
		}
		if id == primaryID {
			return fmt.Errorf("llm: fallback %q equals the primary model", Redact(id))
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("llm: fallback %q is duplicated", Redact(id))
		}
		seen[id] = struct{}{}
		entry, err := r.Get(id)
		if err != nil {
			return fmt.Errorf("llm: fallback %q: %w", Redact(id), err)
		}
		if !entry.AllowedFallback {
			return fmt.Errorf("llm: fallback %q is not allowed_fallback", Redact(id))
		}
	}
	return nil
}
