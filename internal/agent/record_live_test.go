//go:build live

package agent_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestRecordM2Cassettes is the one manual recording step for the three M2
// smoke scenarios. It refuses to run in CI or without an explicit opt-in, and
// never logs the API key or a cassette body.
//
// Run it once with:
//
//	set -a; . ./.env; set +a
//	RECON_RECORD_CASSETTES=1 go test -tags=live -run TestRecordM2Cassettes ./internal/agent -count=1
func TestRecordM2Cassettes(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("recording cassettes is forbidden in CI")
	}
	if os.Getenv("RECON_RECORD_CASSETTES") != "1" {
		t.Skip("set RECON_RECORD_CASSETTES=1 to record M2 cassettes")
	}
	if strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")) == "" {
		t.Skip("OPENCODE_API_KEY is not set")
	}
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Fatal("TEST_DATABASE_URL is required to record M2 cassettes")
	}
	ctx := context.Background()
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		t.Fatalf("ensure template: %v", err)
	}

	cassetteDir := filepath.Join(repoRoot(t), "eval", "cassettes")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// Record with the same fixture ingest config the gate replays, so psp_m2
	// memos are wrapped in the recorded requests.
	cfg.Ingest = m2.IngestConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}

	live := testdb.New(t)
	if err := m2.Load(ctx, live.Pool); err != nil {
		t.Fatalf("load live fixture: %v", err)
	}
	liveApp := openLiveRolePool(t, live, db.RoleApp)
	liveAgent := openLiveRolePool(t, live, db.RoleAgentRO)

	timeSource := clock.Real{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	liveClient, err := llm.NewClient(cfg.LLM, llm.Options{Clock: timeSource, Logger: logger})
	if err != nil {
		t.Fatalf("build live client: %v", err)
	}
	recorded, err := llm.NewCassette(liveClient, llm.CassetteOptions{
		Mode:     llm.CassetteRecord,
		Dir:      cassetteDir,
		Update:   true,
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Now:      timeSource.Now,
	})
	if err != nil {
		t.Fatalf("build recording cassette: %v", err)
	}

	for _, scenario := range m2.Scenarios() {
		report, err := agentrun.Run(ctx, agentrun.Options{
			CaseID:        scenario.CaseID,
			Mode:          agentrun.ModeScripted, // deterministic run ids; Client is the recording wrapper
			PromptVersion: "v1",
		}, agentrun.Deps{
			Config:    cfg,
			App:       liveApp,
			Agent:     liveAgent,
			AgentExec: liveAgent,
			Clock:     timeSource,
			Logger:    logger,
			Client:    recorded,
			NewRunID:  agent.DeterministicRunID,
			WorkerID:  "cli:record:" + scenario.Mode,
		})
		if err != nil {
			t.Fatalf("%s: record run: %v", scenario.Mode, err)
		}
		if report.Outcome != string(agent.OutcomeProposal) {
			t.Fatalf("%s: outcome = %s, want proposal", scenario.Mode, report.Outcome)
		}
		if report.ProposalType != scenario.ExpectedType {
			t.Fatalf("%s: proposal type = %s, want %s", scenario.Mode, report.ProposalType, scenario.ExpectedType)
		}
	}

	files := listCassettes(t, cassetteDir)
	t.Logf("M2 cassettes recorded: %d file(s) in %s", len(files), cassetteDir)
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("stat cassette: %v", err)
		}
		if info.Size() > 1<<20 {
			t.Fatalf("cassette %s exceeds 1 MiB", file)
		}
	}
	if len(files) < len(m2.Scenarios()) {
		t.Fatalf("recorded %d cassettes, want at least %d", len(files), len(m2.Scenarios()))
	}

	// Replay what was just recorded in a second fresh clone. A miss here means
	// the fixture or request shape is not deterministic and the cassettes must
	// not be committed.
	replayDB := testdb.New(t)
	if err := m2.Load(ctx, replayDB.Pool); err != nil {
		t.Fatalf("load replay fixture: %v", err)
	}
	replayApp := openLiveRolePool(t, replayDB, db.RoleApp)
	replayAgent := openLiveRolePool(t, replayDB, db.RoleAgentRO)
	replayCassette, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      cassetteDir,
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
	})
	if err != nil {
		t.Fatalf("build replay cassette: %v", err)
	}
	for _, scenario := range m2.Scenarios() {
		report, err := agentrun.Run(ctx, agentrun.Options{
			CaseID:        scenario.CaseID,
			Mode:          agentrun.ModeScripted,
			PromptVersion: "v1",
		}, agentrun.Deps{
			Config:    cfg,
			App:       replayApp,
			Agent:     replayAgent,
			AgentExec: replayAgent,
			Clock:     timeSource,
			Logger:    logger,
			Client:    replayCassette,
			NewRunID:  agent.DeterministicRunID,
			WorkerID:  "cli:replay:" + scenario.Mode,
		})
		if err != nil {
			if errors.Is(err, llm.ErrCassetteMiss) {
				t.Fatalf("%s: replay miss immediately after recording: %v", scenario.Mode, err)
			}
			t.Fatalf("%s: replay: %v", scenario.Mode, err)
		}
		if report.ProposalType != scenario.ExpectedType {
			t.Fatalf("%s: replay proposal type = %s, want %s", scenario.Mode, report.ProposalType, scenario.ExpectedType)
		}
	}
}

func openLiveRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool.Pool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func listCassettes(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cassettes: %v", err)
	}
	return files
}
