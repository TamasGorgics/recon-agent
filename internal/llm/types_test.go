package llm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func temperature(value int) *float64 { //floatlint:allow provider temperature, not money
	out := float64(value) //floatlint:allow provider temperature, not money
	return &out
}

func differentTemperature() *float64 { //floatlint:allow provider temperature, not money
	value := math.Inf(1)
	return &value
}

func negativeTemperature() *float64 { //floatlint:allow out-of-range test temperature, not money
	value := -1.0 //floatlint:allow out-of-range test temperature, not money
	return &value
}

func TestAPIIDAndSelector(t *testing.T) {
	if got, want := APIID("opencode-go/deepseek-v4.1-flash"), "deepseek-v4.1-flash"; got != want {
		t.Errorf("APIID(selector) = %q, want %q", got, want)
	}
	if got, want := APIID("deepseek-v4.1-flash"), "deepseek-v4.1-flash"; got != want {
		t.Errorf("APIID(id) = %q, want %q", got, want)
	}
	if got, want := Selector("deepseek-v4.1-flash"), "opencode-go/deepseek-v4.1-flash"; got != want {
		t.Errorf("Selector(id) = %q, want %q", got, want)
	}
	if got, want := Selector("opencode-go/deepseek-v4.1-flash"), "opencode-go/deepseek-v4.1-flash"; got != want {
		t.Errorf("Selector(selector) = %q, want %q", got, want)
	}
	if err := ValidateModelID("opencode-go/glm-5.3-flash"); err != nil {
		t.Errorf("ValidateModelID(selector) = %v", err)
	}
	if err := ValidateModelID("has space"); err == nil {
		t.Fatal("ValidateModelID accepted a space")
	}
}

func TestRequestValidate(t *testing.T) {
	validMessage := Message{Role: RoleUser, Content: "hi"}
	object := json.RawMessage(`{"type":"object","properties":{}}`)
	validTool := ToolDef{Name: "get_ping", Parameters: object}

	tests := []struct {
		name string
		req  Request
		want string
	}{
		{"valid nil temperature", Request{Model: "m", System: "s"}, ""},
		{"valid zero temperature", Request{Model: "m", Messages: []Message{validMessage}, Temperature: temperature(0), MaxTokens: 0}, ""},
		{"missing model", Request{System: "s"}, "model"},
		{"missing system and messages", Request{Model: "m"}, "system or messages"},
		{"negative temperature", Request{Model: "m", System: "s", Temperature: negativeTemperature()}, "temperature"},
		{"temperature too high", Request{Model: "m", System: "s", Temperature: differentTemperature()}, "temperature"},
		{"negative max tokens", Request{Model: "m", System: "s", MaxTokens: -1}, "max_tokens"},
		{"empty tool name", Request{Model: "m", System: "s", Tools: []ToolDef{{Parameters: object}}}, "empty name"},
		{"duplicate tool name", Request{Model: "m", System: "s", Tools: []ToolDef{validTool, validTool}}, "duplicated"},
		{"nil parameters", Request{Model: "m", System: "s", Tools: []ToolDef{{Name: "x"}}}, "parameters"},
		{"array parameters", Request{Model: "m", System: "s", Tools: []ToolDef{{Name: "x", Parameters: json.RawMessage(`[]`)}}}, "parameters"},
		{"invalid parameters", Request{Model: "m", System: "s", Tools: []ToolDef{{Name: "x", Parameters: json.RawMessage(`{`)}}}, "parameters"},
		{"choice without tools", Request{Model: "m", System: "s", ToolChoice: &ToolChoice{Mode: ToolChoiceAuto}}, "requires at least one tool"},
		{"bad choice mode", Request{Model: "m", System: "s", Tools: []ToolDef{validTool}, ToolChoice: &ToolChoice{Mode: "sometimes"}}, "invalid"},
		{"tool choice without name", Request{Model: "m", System: "s", Tools: []ToolDef{validTool}, ToolChoice: &ToolChoice{Mode: ToolChoiceTool}}, "requires a name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestTemperaturePointerPreservesZero(t *testing.T) {
	req := Request{Model: "m", System: "s", Temperature: temperature(0)}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatalf("Temperature = %v, want pointer to 0", req.Temperature)
	}
	raw, err := json.Marshal(req.Temperature)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "0" {
		t.Fatalf("temperature JSON = %s, want 0", raw)
	}
}
