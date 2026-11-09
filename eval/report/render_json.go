package report

import "encoding/json"

// RenderJSON renders the machine report with stable indentation and a trailing
// newline. generated_at is the only volatile field; goldens pin the clock.
func RenderJSON(report Report) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
