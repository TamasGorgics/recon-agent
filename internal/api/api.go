package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// IngestService is the ingest pipeline as seen by the HTTP handler. It is
// satisfied by *ingest.Ingester.
type IngestService interface {
	Ingest(ctx context.Context, req ingest.Request) (ingest.Result, error)
}

// MatchService runs the matcher for one scope and window. It is satisfied by
// *match.Engine.
type MatchService interface {
	Run(ctx context.Context, req match.RunRequest) (match.RunResult, error)
}

// CaseService persists break candidates and creates cases. It is satisfied by
// *cases.Service.
type CaseService interface {
	CreateBreaks(ctx context.Context, req cases.CreateBreaksRequest) (cases.CreateBreaksResult, error)
}

// CaseReader lists case summaries. It is satisfied by *PostgresStore.
type CaseReader interface {
	ListCases(ctx context.Context, query CaseListQuery) (CaseListPage, error)
}

// CaseFileReader reads one case-file document in a single read-only
// transaction. It is satisfied by *PostgresStore.
type CaseFileReader interface {
	GetCaseFile(ctx context.Context, caseID string) (CaseFile, error)
}

// ProposalReader lists proposal summaries. It is satisfied by *PostgresStore.
type ProposalReader interface {
	ListProposals(ctx context.Context, query ProposalListQuery) (ProposalListPage, error)
}

// SwitchService updates one seeded switch and appends the matching audit entry.
// It is satisfied by *SwitchWriter.
type SwitchService interface {
	SetSwitch(ctx context.Context, req SwitchSetRequest) (SwitchResult, error)
}

// Options wires the API. Config and Keys come from the validated runtime
// configuration; Keys holds digests only. All services are required.
type Options struct {
	Config config.APIConfig
	Keys   []config.APIKey
	Logger *slog.Logger
	Clock  clock.Clock

	Ingest     IngestService
	Matcher    MatchService
	Cases      CaseService
	CaseReader CaseReader
	CaseFile   CaseFileReader
	Proposals  ProposalReader
	Switches   SwitchService
}

// API owns the router and its dependencies. It is immutable after New.
type API struct {
	cfg    config.APIConfig
	keys   []config.APIKey
	logger *slog.Logger
	clock  clock.Clock

	ingest    IngestService
	matcher   MatchService
	cases     CaseService
	caseList  CaseReader
	caseFile  CaseFileReader
	proposals ProposalReader
	switches  SwitchService

	handler http.Handler
}

// New validates the wiring and builds the router. It never records or formats
// key material: errors from this function name only fields.
func New(opts Options) (*API, error) {
	cfg := opts.Config.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(opts.Keys) == 0 {
		return nil, errors.New("api: at least one API key is required")
	}
	keys := make([]config.APIKey, len(opts.Keys))
	for i, key := range opts.Keys {
		if key.ID == "" {
			return nil, fmt.Errorf("api: API key %d has an empty id", i)
		}
		if !key.Role.Valid() {
			return nil, fmt.Errorf("api: API key %d has an invalid role", i)
		}
		if key.Digest == ([sha256.Size]byte{}) {
			return nil, fmt.Errorf("api: API key %d has an empty digest", i)
		}
		keys[i] = key
	}
	if opts.Ingest == nil {
		return nil, errors.New("api: ingest service is required")
	}
	if opts.Matcher == nil {
		return nil, errors.New("api: match service is required")
	}
	if opts.Cases == nil {
		return nil, errors.New("api: case service is required")
	}
	if opts.CaseReader == nil {
		return nil, errors.New("api: case reader is required")
	}
	if opts.CaseFile == nil {
		return nil, errors.New("api: case file reader is required")
	}
	if opts.Proposals == nil {
		return nil, errors.New("api: proposal reader is required")
	}
	if opts.Switches == nil {
		return nil, errors.New("api: switch service is required")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	a := &API{
		cfg:       cfg,
		keys:      keys,
		logger:    logger,
		clock:     clk,
		ingest:    opts.Ingest,
		matcher:   opts.Matcher,
		cases:     opts.Cases,
		caseList:  opts.CaseReader,
		caseFile:  opts.CaseFile,
		proposals: opts.Proposals,
		switches:  opts.Switches,
	}
	a.handler = a.buildRouter()
	return a, nil
}

// Handler returns the fully wrapped router.
func (a *API) Handler() http.Handler { return a.handler }

func (a *API) buildRouter() http.Handler {
	router := chi.NewRouter()
	// Outermost first: every response has a request ID and trace ID. The
	// access log must wrap recovery so a recovered panic is observed as the
	// committed 500, not as an untouched 200.
	router.Use(a.withRequestContext)
	router.Use(a.accessLog)
	router.Use(a.recovery)

	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, http.StatusNotFound, codeNotFound, messageNotFound)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if allow := a.allowedMethods(r.URL.Path); len(allow) > 0 {
			w.Header().Set("Allow", allow)
		}
		a.writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, messageMethodNotAllowed)
	})

	for _, route := range a.routeTable() {
		var handler http.Handler = route.Handler
		if route.Role != "" {
			handler = a.authenticate(route.Role, handler)
		}
		router.Method(route.Method, route.Pattern, handler)
	}
	return router
}
