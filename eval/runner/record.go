package runner

import (
	"errors"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// newRecordClient builds the production live client and wraps it in cassette
// record mode. The cassette layer applies llm.Redact before writing and refuses
// to overwrite unless update is true.
func newRecordClient(cfg config.LLMConfig, dir string, update bool, opts llm.Options) (llm.Client, error) {
	inner, err := llm.NewClient(cfg, opts)
	if err != nil {
		return nil, err
	}
	return wrapRecorder(inner, cfg, dir, update, opts.Clock)
}

// wrapRecorder is the runner's single record-mode construction point. Tests
// inject a fake-server adapter here so the cassette path mirrors production.
func wrapRecorder(inner llm.Client, cfg config.LLMConfig, dir string, update bool,
	timeSource clock.Clock) (llm.Client, error) {
	if inner == nil {
		return nil, errors.New("eval/runner: record mode requires a live client")
	}
	if strings.TrimSpace(dir) == "" {
		dir = llm.DefaultCassetteDir
	}
	if timeSource == nil {
		timeSource = clock.Real{}
	}
	return llm.NewCassette(inner, llm.CassetteOptions{
		Mode:     llm.CassetteRecord,
		Dir:      dir,
		Update:   update,
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		Now:      timeSource.Now,
	})
}

// resolveCassetteDir selects the cassette root. An explicit CLI flag wins, then
// a programmatic Deps.CassetteDir (tests), then the option/default value
// (including a value restored from a resumed run).
func resolveCassetteDir(opts Options, deps Deps) string {
	if opts.CassetteDirSet && strings.TrimSpace(opts.CassetteDir) != "" {
		return strings.TrimSpace(opts.CassetteDir)
	}
	if dir := strings.TrimSpace(deps.CassetteDir); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(opts.CassetteDir); dir != "" {
		return dir
	}
	return llm.DefaultCassetteDir
}
