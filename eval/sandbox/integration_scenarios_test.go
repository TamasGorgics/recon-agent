//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// databaseFromDSN returns the admin DSN's database path, defaulting to
// postgres. It lives with the integration tests because production code always
// derives a clone database name explicitly.
func databaseFromDSN(dsn string) (string, error) {
	parsed, err := parseAdminDSN(dsn)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" {
		name = "postgres"
	}
	return name, nil
}

func TestSandboxWorldLoadAndMapping(t *testing.T) {
	ctx := context.Background()
	const seed = 7202
	ds, cfg := miniDataset(t, seed)
	manager := newManager(t)
	sandbox, err := manager.Clone(ctx, ds, LoadOptions{Config: cfg})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(func() {
		if err := sandbox.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if got, want := queryInt(t, sandbox.Admin, `SELECT count(*) FROM eval_truth.scenarios`),
		len(ds.Truth.Scenarios); got != want {
		t.Fatalf("eval_truth.scenarios = %d, want %d", got, want)
	}
	if got := queryInt(t, sandbox.Admin, `SELECT count(*) FROM eval_truth.sandbox_meta`); got != 1 {
		t.Fatalf("eval_truth.sandbox_meta = %d rows, want 1", got)
	}
	var datasetVersion, split string
	var metaSeed int64
	if err := sandbox.Admin.QueryRow(ctx,
		`SELECT eval_dataset_version, split, seed FROM eval_truth.sandbox_meta`).
		Scan(&datasetVersion, &split, &metaSeed); err != nil {
		t.Fatalf("read sandbox_meta: %v", err)
	}
	if datasetVersion != ds.Manifest.EvalDatasetVersion || split != string(ds.Manifest.Split) ||
		metaSeed != ds.Manifest.Seed {
		t.Fatalf("sandbox_meta = (%s, %s, %d), want (%s, %s, %d)",
			datasetVersion, split, metaSeed, ds.Manifest.EvalDatasetVersion,
			ds.Manifest.Split, ds.Manifest.Seed)
	}

	expectedScoreable := 0
	for _, truth := range ds.Truth.Scenarios {
		if truth.BreakSignature != "" || truth.Mode == modes.InjectionMemo {
			expectedScoreable++
		}
	}
	if got := queryInt(t, sandbox.Admin, `SELECT count(*) FROM eval_truth.scenario_cases`); got != expectedScoreable {
		t.Fatalf("eval_truth.scenario_cases = %d, want %d", got, expectedScoreable)
	}

	scopes := allScopes(seed, cfg.Synth)
	refs, err := sandbox.ScoreableScenarios(ctx)
	if err != nil {
		t.Fatalf("ScoreableScenarios: %v", err)
	}
	if len(refs) != expectedScoreable {
		t.Fatalf("ScoreableScenarios = %d, want %d", len(refs), expectedScoreable)
	}
	if !sort.SliceIsSorted(refs, func(i, j int) bool {
		return refs[i].ScenarioID < refs[j].ScenarioID
	}) {
		t.Fatal("ScoreableScenarios is not sorted by scenario id")
	}

	refByScenario := make(map[string]CaseRef, len(refs))
	caseIDs := make(map[string]string, len(refs))
	for _, ref := range refs {
		refByScenario[ref.ScenarioID] = ref
		if previous, duplicate := caseIDs[ref.CaseID]; duplicate {
			t.Fatalf("case %s is shared by scenarios %s and %s",
				ref.CaseID, previous, ref.ScenarioID)
		}
		caseIDs[ref.CaseID] = ref.ScenarioID
		if ref.ScenarioTime.IsZero() {
			t.Fatalf("scenario %s has a zero scenario time", ref.ScenarioID)
		}
		if ref.ScenarioTime.Location() != time.UTC {
			t.Fatalf("scenario %s time location = %s, want UTC",
				ref.ScenarioID, ref.ScenarioTime.Location())
		}
		var persisted time.Time
		if err := sandbox.Admin.QueryRow(ctx,
			`SELECT scenario_time FROM eval_truth.scenario_cases WHERE scenario_id = $1`,
			ref.ScenarioID).Scan(&persisted); err != nil {
			t.Fatalf("read persisted scenario time %s: %v", ref.ScenarioID, err)
		}
		if !persisted.Equal(ref.ScenarioTime) {
			t.Fatalf("scenario %s persisted time %s != returned %s",
				ref.ScenarioID, persisted, ref.ScenarioTime)
		}
	}

	batchCaseIDs := make(map[string]struct{})
	for _, truth := range ds.Truth.Scenarios {
		scoreable := truth.BreakSignature != "" || truth.Mode == modes.InjectionMemo
		ref, err := sandbox.CaseForScenario(ctx, truth.ScenarioID)
		if !scoreable {
			if !errors.Is(err, ErrNotScoreable) {
				t.Fatalf("clean scenario %s error = %v, want ErrNotScoreable", truth.ScenarioID, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("CaseForScenario(%s): %v", truth.ScenarioID, err)
		}
		if ref.CaseID == "" {
			t.Fatalf("scenario %s has an empty case id", truth.ScenarioID)
		}
		var caseStatus string
		if err := sandbox.Admin.QueryRow(ctx,
			`SELECT status FROM cases WHERE id = $1::uuid`, ref.CaseID).Scan(&caseStatus); err != nil {
			t.Fatalf("read case %s for scenario %s: %v", ref.CaseID, truth.ScenarioID, err)
		}
		if caseStatus != "open" {
			t.Fatalf("case %s status = %q, want open", ref.CaseID, caseStatus)
		}

		if truth.Mode == modes.InjectionMemo {
			if !ref.Synthetic {
				t.Fatalf("injection scenario %s is not marked synthetic", truth.ScenarioID)
			}
			if ref.BreakID != "" {
				t.Fatalf("synthetic scenario %s has break_id %q", truth.ScenarioID, ref.BreakID)
			}
			var synthetic *bool
			if err := sandbox.Admin.QueryRow(ctx,
				`SELECT synthetic FROM eval_truth.scenario_cases WHERE scenario_id = $1`,
				truth.ScenarioID).Scan(&synthetic); err != nil {
				t.Fatalf("read synthetic flag %s: %v", truth.ScenarioID, err)
			}
			if synthetic == nil || !*synthetic {
				t.Fatalf("scenario %s synthetic flag = %v, want true", truth.ScenarioID, synthetic)
			}
			expectedCandidate, err := syntheticCandidate(truth, scopes)
			if err != nil {
				t.Fatalf("synthetic candidate %s: %v", truth.ScenarioID, err)
			}
			var signature string
			if err := sandbox.Admin.QueryRow(ctx,
				`SELECT b.signature
				   FROM case_breaks cb
				   JOIN breaks b ON b.id = cb.break_id
				  WHERE cb.case_id = $1::uuid`, ref.CaseID).Scan(&signature); err != nil {
				t.Fatalf("read synthetic break for %s: %v", truth.ScenarioID, err)
			}
			if signature != expectedCandidate.Signature() {
				t.Fatalf("synthetic break signature = %s, want %s",
					signature, expectedCandidate.Signature())
			}
			continue
		}

		if ref.Synthetic {
			t.Fatalf("scenario %s is unexpectedly synthetic", truth.ScenarioID)
		}
		if ref.BreakID == "" {
			t.Fatalf("scenario %s has an empty break id", truth.ScenarioID)
		}
		var signature string
		if err := sandbox.Admin.QueryRow(ctx,
			`SELECT b.signature
			   FROM case_breaks cb
			   JOIN breaks b ON b.id = cb.break_id
			  WHERE cb.case_id = $1::uuid`, ref.CaseID).Scan(&signature); err != nil {
			t.Fatalf("read break for scenario %s: %v", truth.ScenarioID, err)
		}
		if signature != truth.BreakSignature {
			t.Fatalf("scenario %s break signature = %s, truth %s",
				truth.ScenarioID, signature, truth.BreakSignature)
		}
		if truth.Mode == modes.BatchComposite {
			batchCaseIDs[ref.CaseID] = struct{}{}
		}
	}
	if len(batchCaseIDs) != 2 {
		t.Fatalf("batch_composite created %d distinct cases, want 2", len(batchCaseIDs))
	}

	if _, err := sandbox.CaseForScenario(ctx, "missing-scenario"); !errors.Is(err, ErrScenarioNotFound) {
		t.Fatalf("unknown scenario error = %v, want ErrScenarioNotFound", err)
	}

	if queryInt(t, sandbox.Admin, `SELECT count(*) FROM ledger_txns`) == 0 {
		t.Fatal("world loaded no ledger transactions")
	}
	if queryInt(t, sandbox.Admin, `SELECT count(*) FROM external_records`) == 0 {
		t.Fatal("world loaded no external records")
	}
	if queryInt(t, sandbox.Admin, `SELECT count(*) FROM cases`) == 0 {
		t.Fatal("world loaded no cases")
	}
}

func TestSandboxClockDeterministicAcrossClones(t *testing.T) {
	ctx := context.Background()
	const seed = 7203
	ds, cfg := miniDataset(t, seed)
	manager := newManager(t)

	first, err := manager.Clone(ctx, ds, LoadOptions{Config: cfg})
	if err != nil {
		t.Fatalf("first Clone: %v", err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	second, err := manager.Clone(ctx, ds, LoadOptions{Config: cfg})
	if err != nil {
		t.Fatalf("second Clone: %v", err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })

	firstRefs, err := first.ScoreableScenarios(ctx)
	if err != nil {
		t.Fatalf("first ScoreableScenarios: %v", err)
	}
	secondRefs, err := second.ScoreableScenarios(ctx)
	if err != nil {
		t.Fatalf("second ScoreableScenarios: %v", err)
	}
	if len(firstRefs) != len(secondRefs) {
		t.Fatalf("clone scoreable counts differ: %d vs %d", len(firstRefs), len(secondRefs))
	}
	secondByScenario := make(map[string]CaseRef, len(secondRefs))
	for _, ref := range secondRefs {
		secondByScenario[ref.ScenarioID] = ref
	}
	for _, ref := range firstRefs {
		other, ok := secondByScenario[ref.ScenarioID]
		if !ok {
			t.Fatalf("scenario %s missing from second clone", ref.ScenarioID)
		}
		if !ref.ScenarioTime.Equal(other.ScenarioTime) {
			t.Fatalf("scenario %s time differs across clones: %s vs %s",
				ref.ScenarioID, ref.ScenarioTime, other.ScenarioTime)
		}
	}
}

func TestSandboxParallelIsolation(t *testing.T) {
	ctx := context.Background()
	dsA, cfgA := miniDataset(t, 7304)
	dsB, cfgB := miniDataset(t, 7305)
	managerA := newManager(t)
	managerB := newManager(t)

	type result struct {
		sandbox *Sandbox
		err     error
	}
	resultsA := make(chan result, 1)
	resultsB := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sandbox, err := managerA.Clone(ctx, dsA, LoadOptions{Config: cfgA})
		resultsA <- result{sandbox: sandbox, err: err}
	}()
	go func() {
		defer wg.Done()
		sandbox, err := managerB.Clone(ctx, dsB, LoadOptions{Config: cfgB})
		resultsB <- result{sandbox: sandbox, err: err}
	}()
	wg.Wait()
	close(resultsA)
	close(resultsB)

	first := <-resultsA
	second := <-resultsB
	if first.err != nil || second.err != nil {
		t.Fatalf("parallel Clone errors: %v / %v", first.err, second.err)
	}
	t.Cleanup(func() { _ = first.sandbox.Close(context.Background()) })
	t.Cleanup(func() { _ = second.sandbox.Close(context.Background()) })
	if first.sandbox.Name == second.sandbox.Name {
		t.Fatalf("both sandboxes use database %q", first.sandbox.Name)
	}

	firstRefs, err := first.sandbox.ScoreableScenarios(ctx)
	if err != nil {
		t.Fatalf("sandbox A ScoreableScenarios: %v", err)
	}
	secondRefs, err := second.sandbox.ScoreableScenarios(ctx)
	if err != nil {
		t.Fatalf("sandbox B ScoreableScenarios: %v", err)
	}
	if len(firstRefs) == 0 || len(secondRefs) == 0 {
		t.Fatal("a parallel sandbox has no scoreable scenarios")
	}
	firstCaseIDs := make(map[string]struct{}, len(firstRefs))
	for _, ref := range firstRefs {
		firstCaseIDs[ref.CaseID] = struct{}{}
	}
	for _, ref := range secondRefs {
		if _, collision := firstCaseIDs[ref.CaseID]; collision {
			t.Fatalf("case id %s appears in both sandboxes", ref.CaseID)
		}
	}

	scenarioA := firstRefs[0].ScenarioID
	if got := queryInt(t, first.sandbox.Admin,
		`SELECT count(*) FROM eval_truth.scenarios WHERE scenario_id = $1`, scenarioA); got != 1 {
		t.Fatalf("scenario A row count in A = %d, want 1", got)
	}
	if got := queryInt(t, second.sandbox.Admin,
		`SELECT count(*) FROM eval_truth.scenarios WHERE scenario_id = $1`, scenarioA); got != 0 {
		t.Fatalf("scenario A row count in B = %d, want 0", got)
	}

	nameA, nameB := first.sandbox.Name, second.sandbox.Name
	loginsA := loginNames(first.sandbox)
	loginsB := loginNames(second.sandbox)
	if err := first.sandbox.Close(ctx); err != nil {
		t.Fatalf("close sandbox A: %v", err)
	}
	if err := second.sandbox.Close(ctx); err != nil {
		t.Fatalf("close sandbox B: %v", err)
	}
	adminDSN := integrationAdminDSN(t)
	for _, name := range []string{nameA, nameB} {
		if databaseExists(t, adminDSN, name) {
			t.Fatalf("parallel clone %q survived Close", name)
		}
	}
	for _, login := range append(loginsA, loginsB...) {
		if roleExists(t, adminDSN, login) {
			t.Fatalf("parallel login %q survived Close", login)
		}
	}
}

func loginNames(sandbox *Sandbox) []string {
	names := make([]string, 0, len(sandbox.logins))
	for _, login := range sandbox.logins {
		names = append(names, login.Username)
	}
	sort.Strings(names)
	return names
}

func TestSandboxCleanupOrphansAndTestNamespaceSafety(t *testing.T) {
	ctx := context.Background()
	adminDSN := integrationAdminDSN(t)
	manager := newManager(t)
	if _, err := manager.EnsureTemplate(ctx); err != nil {
		t.Fatalf("EnsureTemplate: %v", err)
	}

	suffix, err := randomToken(4)
	if err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	leakedDatabase := fmt.Sprintf("recon_eval_%d_9001_%s", os.Getpid(), suffix)
	testDatabase := fmt.Sprintf("recon_test_%d_keep_%s", os.Getpid(), suffix)
	leakedLogin := fmt.Sprintf("recon_eval_login_%d_9001_%s_app", os.Getpid(), suffix)
	testLogin := fmt.Sprintf("recon_test_login_%d_keep_%s_app", os.Getpid(), suffix)

	execAdmin(t, adminDSN, "CREATE DATABASE "+quoteIdent(leakedDatabase))
	execAdmin(t, adminDSN, "CREATE DATABASE "+quoteIdent(testDatabase))
	execAdmin(t, adminDSN, "CREATE ROLE "+quoteIdent(leakedLogin)+" NOLOGIN")
	execAdmin(t, adminDSN, "CREATE ROLE "+quoteIdent(testLogin)+" NOLOGIN")
	t.Cleanup(func() {
		execAdmin(t, adminDSN, "DROP DATABASE IF EXISTS "+quoteIdent(testDatabase)+" WITH (FORCE)")
		execAdmin(t, adminDSN, "DROP ROLE IF EXISTS "+quoteIdent(testLogin))
	})

	dropped, err := CleanupOrphans(ctx, adminDSN, DefaultTemplate)
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("CleanupOrphans dropped %d databases, want at least 1", dropped)
	}
	if databaseExists(t, adminDSN, leakedDatabase) {
		t.Fatalf("leaked eval database %q survived cleanup", leakedDatabase)
	}
	if !databaseExists(t, adminDSN, testDatabase) {
		t.Fatalf("CleanupOrphans touched recon_test_ database %q", testDatabase)
	}
	if !databaseExists(t, adminDSN, DefaultTemplate) {
		t.Fatalf("CleanupOrphans dropped the eval template %q", DefaultTemplate)
	}

	droppedLogins, err := CleanupOrphanLogins(ctx, adminDSN)
	if err != nil {
		t.Fatalf("CleanupOrphanLogins: %v", err)
	}
	if droppedLogins < 1 {
		t.Fatalf("CleanupOrphanLogins dropped %d roles, want at least 1", droppedLogins)
	}
	if roleExists(t, adminDSN, leakedLogin) {
		t.Fatalf("leaked eval login %q survived cleanup", leakedLogin)
	}
	if !roleExists(t, adminDSN, testLogin) {
		t.Fatalf("CleanupOrphanLogins touched recon_test_ role %q", testLogin)
	}
}

func TestSandboxTemplateEnsureAndRebuild(t *testing.T) {
	ctx := context.Background()
	adminDSN := integrationAdminDSN(t)
	suffix, err := randomToken(4)
	if err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	template := fmt.Sprintf("recon_eval_template_t_%d_%s", os.Getpid(), suffix)
	t.Cleanup(func() {
		execAdmin(t, adminDSN, "DROP DATABASE IF EXISTS "+quoteIdent(template)+" WITH (FORCE)")
	})

	first, err := NewManager(Options{AdminDSN: adminDSN, Template: template})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	created, err := first.EnsureTemplate(ctx)
	if err != nil || !created {
		t.Fatalf("first EnsureTemplate = (%t, %v), want (true, nil)", created, err)
	}
	second, err := NewManager(Options{AdminDSN: adminDSN, Template: template})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	created, err = second.EnsureTemplate(ctx)
	if err != nil || created {
		t.Fatalf("reused EnsureTemplate = (%t, %v), want (false, nil)", created, err)
	}

	templateDSN, err := dsnForDatabase(adminDSN, template)
	if err != nil {
		t.Fatalf("template DSN: %v", err)
	}
	conn, err := pgx.Connect(ctx, templateDSN)
	if err != nil {
		t.Fatalf("connect template: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE goose_db_version
		    SET version_id = version_id - 1
		  WHERE version_id = (SELECT max(version_id) FROM goose_db_version)`); err != nil {
		conn.Close(ctx)
		t.Fatalf("make template stale: %v", err)
	}
	_ = conn.Close(ctx)

	third, err := NewManager(Options{AdminDSN: adminDSN, Template: template})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	created, err = third.EnsureTemplate(ctx)
	if err != nil || !created {
		t.Fatalf("stale EnsureTemplate = (%t, %v), want rebuild (true, nil)", created, err)
	}
	conn, err = pgx.Connect(ctx, templateDSN)
	if err != nil {
		t.Fatalf("connect rebuilt template: %v", err)
	}
	var hasEvalTruth bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('eval_truth.scenarios') IS NOT NULL`).Scan(&hasEvalTruth); err != nil {
		conn.Close(ctx)
		t.Fatalf("read rebuilt template schema: %v", err)
	}
	_ = conn.Close(ctx)
	if !hasEvalTruth {
		t.Fatal("rebuilt template has no eval_truth.scenarios")
	}

	concurrentTemplate := template + "_c"
	t.Cleanup(func() {
		execAdmin(t, adminDSN, "DROP DATABASE IF EXISTS "+quoteIdent(concurrentTemplate)+" WITH (FORCE)")
	})
	var wg sync.WaitGroup
	results := make(chan struct {
		created bool
		err     error
	}, 3)
	for i := 0; i < 3; i++ {
		manager, err := NewManager(Options{AdminDSN: adminDSN, Template: concurrentTemplate})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := manager.EnsureTemplate(ctx)
			results <- struct {
				created bool
				err     error
			}{created: created, err: err}
		}()
	}
	wg.Wait()
	close(results)
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent EnsureTemplate: %v", result.err)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("concurrent EnsureTemplate created %d templates, want 1", createdCount)
	}
}

func TestSandboxMaintenancePrivilegesFailFast(t *testing.T) {
	ctx := context.Background()
	adminDSN := integrationAdminDSN(t)
	suffix, err := randomToken(4)
	if err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	role := fmt.Sprintf("recon_low_priv_%d_%s", os.Getpid(), suffix)
	password := "low-priv-password-" + suffix
	execAdmin(t, adminDSN, "CREATE ROLE "+quoteIdent(role)+" LOGIN PASSWORD "+quoteLiteral(password))
	t.Cleanup(func() {
		execAdmin(t, adminDSN, "DROP ROLE IF EXISTS "+quoteIdent(role))
	})
	database, err := databaseFromDSN(adminDSN)
	if err != nil {
		t.Fatalf("admin database: %v", err)
	}
	lowDSN, err := dsnForLogin(adminDSN, database, role, password)
	if err != nil {
		t.Fatalf("low-priv DSN: %v", err)
	}
	manager, err := NewManager(Options{AdminDSN: lowDSN})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.EnsureTemplate(ctx); err == nil {
		t.Fatal("EnsureTemplate succeeded without CREATEDB/CREATEROLE")
	} else if !strings.Contains(err.Error(), "CREATEDB") || !strings.Contains(err.Error(), "CREATEROLE") {
		t.Fatalf("privilege error = %v, want CREATEDB and CREATEROLE named", err)
	}
}
