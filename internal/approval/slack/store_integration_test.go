//go:build integration

package slack_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "approval/slack: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "approval/slack: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

var storeProposalCounter atomic.Int64

type storeFixture struct {
	t   *testing.T
	d   *testdb.DB
	app *pgxpool.Pool
}

func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	d := testdb.New(t)
	role, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(role.Close)
	return &storeFixture{t: t, d: d, app: role.Pool}
}

func (f *storeFixture) newProposal() string {
	f.t.Helper()
	var caseID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases (priority, value_minor, currency, created_at)
		 VALUES (0, 100, 'USD', now())
		 RETURNING id::text`).Scan(&caseID); err != nil {
		f.t.Fatalf("create case: %v", err)
	}
	var runID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		f.t.Fatalf("create agent run: %v", err)
	}
	var proposalID string
	key := fmt.Sprintf("m4-12-store-%d-%d", os.Getpid(), storeProposalCounter.Add(1))
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, 'link', '{}'::jsonb, 'evidence matches',
		         0.9000, 'pending_approval', 'state-hash', $3, now())
		 RETURNING id::text`, caseID, runID, key).Scan(&proposalID); err != nil {
		f.t.Fatalf("create proposal: %v", err)
	}
	return proposalID
}

func TestStoreGetMissingAndValidation(t *testing.T) {
	f := newStoreFixture(t)
	store := &slack.Store{}
	ctx := context.Background()
	proposalID := f.newProposal()

	message, found, err := store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil {
		t.Fatalf("Get missing row: %v", err)
	}
	if found {
		t.Fatalf("Get found a missing row: %+v", message)
	}

	if _, err := store.GetRequired(ctx, f.app, proposalID, approval.ChannelSlack); !errors.Is(err, slack.ErrMessageNotFound) {
		t.Fatalf("GetRequired error = %v, want ErrMessageNotFound", err)
	}

	invalid := []struct {
		name       string
		proposalID string
		channel    string
	}{
		{"upper-case uuid", strings.ToUpper(proposalID), approval.ChannelSlack},
		{"malformed uuid", "not-a-uuid", approval.ChannelSlack},
		{"empty uuid", "", approval.ChannelSlack},
		{"empty channel", proposalID, ""},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := store.Get(ctx, f.app, test.proposalID, test.channel); !errors.Is(err, slack.ErrInvalidMessage) {
				t.Fatalf("Get error = %v, want ErrInvalidMessage", err)
			}
			if _, err := store.InsertPosted(ctx, f.app, test.proposalID, test.channel,
				"C0123456789:1732041600.123456"); !errors.Is(err, slack.ErrInvalidMessage) {
				t.Fatalf("InsertPosted error = %v, want ErrInvalidMessage", err)
			}
		})
	}

	var typedNil *pgxpool.Pool
	if _, _, err := store.Get(ctx, typedNil, proposalID, approval.ChannelSlack); !errors.Is(err, slack.ErrInvalidMessage) {
		t.Fatalf("typed-nil Get error = %v, want ErrInvalidMessage", err)
	}
}

func TestStoreInsertPostedIdempotent(t *testing.T) {
	f := newStoreFixture(t)
	store := &slack.Store{}
	ctx := context.Background()
	proposalID := f.newProposal()

	inserted, err := store.InsertPosted(ctx, f.app, proposalID, approval.ChannelSlack,
		"C0123456789:1732041600.123456")
	if err != nil {
		t.Fatalf("InsertPosted: %v", err)
	}
	if !inserted {
		t.Fatal("InsertPosted inserted=false on a fresh row")
	}

	message, found, err := store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil {
		t.Fatalf("Get posted row: %v", err)
	}
	if !found {
		t.Fatal("Get did not find the posted row")
	}
	if message.ProposalID != proposalID || message.Channel != approval.ChannelSlack ||
		message.ExternalRef != "C0123456789:1732041600.123456" ||
		message.State != slack.StatePosted {
		t.Fatalf("posted message = %+v", message)
	}
	if message.CreatedAt.IsZero() || message.UpdatedAt.IsZero() {
		t.Fatalf("database-clock timestamps are zero: %+v", message)
	}
	if !message.CreatedAt.Equal(message.UpdatedAt) {
		t.Fatalf("insert timestamps = %s/%s, want the same statement now()",
			message.CreatedAt, message.UpdatedAt)
	}
	wantUpdatedAt := message.UpdatedAt

	// A duplicate insert is a no-op and must not rewrite the stored ref/time.
	inserted, err = store.InsertPosted(ctx, f.app, proposalID, approval.ChannelSlack,
		"C9999999999:9999999999.999999")
	if err != nil {
		t.Fatalf("duplicate InsertPosted: %v", err)
	}
	if inserted {
		t.Fatal("duplicate InsertPosted inserted=true")
	}
	message, found, err = store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil || !found {
		t.Fatalf("Get after duplicate: found=%t err=%v", found, err)
	}
	if message.ExternalRef != "C0123456789:1732041600.123456" || !message.UpdatedAt.Equal(wantUpdatedAt) {
		t.Fatalf("duplicate insert changed the row: %+v", message)
	}

	// The same proposal may have one row per channel.
	inserted, err = store.InsertPosted(ctx, f.app, proposalID, approval.ChannelTicket,
		"ticket:42")
	if err != nil {
		t.Fatalf("InsertPosted ticket: %v", err)
	}
	if !inserted {
		t.Fatal("InsertPosted ticket inserted=false")
	}

	if _, err := store.InsertPosted(ctx, f.app, proposalID, "other", ""); !errors.Is(err, slack.ErrInvalidMessage) {
		t.Fatalf("empty external ref error = %v, want ErrInvalidMessage", err)
	}
}

func TestStoreTransitions(t *testing.T) {
	f := newStoreFixture(t)
	store := &slack.Store{}
	ctx := context.Background()
	proposalID := f.newProposal()

	if _, err := store.InsertPosted(ctx, f.app, proposalID, approval.ChannelSlack,
		"C0123456789:1732041600.123456"); err != nil {
		t.Fatalf("InsertPosted: %v", err)
	}

	// Backdate both columns through the maintenance pool. A transition must
	// overwrite updated_at with the database clock, not a caller timestamp.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.d.Pool.Exec(ctx,
		`UPDATE approval_messages SET created_at = $3, updated_at = $3
		  WHERE proposal_id = $1::uuid AND channel = $2`,
		proposalID, approval.ChannelSlack, old); err != nil {
		t.Fatalf("backdate message: %v", err)
	}

	changed, err := store.MarkUpdated(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil {
		t.Fatalf("MarkUpdated: %v", err)
	}
	if !changed {
		t.Fatal("MarkUpdated changed=false on a posted row")
	}
	message, found, err := store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil || !found {
		t.Fatalf("Get updated: found=%t err=%v", found, err)
	}
	if message.State != slack.StateUpdated {
		t.Fatalf("updated message = %+v", message)
	}
	if !message.CreatedAt.Equal(old) {
		t.Fatalf("MarkUpdated rewrote created_at to %s, want %s", message.CreatedAt, old)
	}
	if !message.UpdatedAt.After(old) {
		t.Fatalf("MarkUpdated updated_at = %s, want a database now() after %s",
			message.UpdatedAt, old)
	}
	updatedAt := message.UpdatedAt

	// Repeating the transition is an idempotent no-op and keeps the timestamp.
	changed, err = store.MarkUpdated(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil {
		t.Fatalf("repeat MarkUpdated: %v", err)
	}
	if changed {
		t.Fatal("repeat MarkUpdated changed=true")
	}
	message, _, _ = store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if !message.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("repeat MarkUpdated moved updated_at to %s", message.UpdatedAt)
	}

	// Backdate again so the cancel transition also proves it uses now().
	if _, err := f.d.Pool.Exec(ctx,
		`UPDATE approval_messages SET updated_at = $3
		  WHERE proposal_id = $1::uuid AND channel = $2`,
		proposalID, approval.ChannelSlack, old); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
	changed, err = store.MarkCancelled(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil {
		t.Fatalf("MarkCancelled: %v", err)
	}
	if !changed {
		t.Fatal("MarkCancelled changed=false on an updated row")
	}
	message, _, _ = store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if message.State != slack.StateCancelled || !message.UpdatedAt.After(old) {
		t.Fatalf("cancelled message = %+v, want database-clock updated_at", message)
	}
	cancelledAt := message.UpdatedAt

	// Cancelled is terminal: later updates/cancels are no-ops.
	for name, transition := range map[string]func() (bool, error){
		"repeat cancel": func() (bool, error) {
			return store.MarkCancelled(ctx, f.app, proposalID, approval.ChannelSlack)
		},
		"update after cancel": func() (bool, error) {
			return store.MarkUpdated(ctx, f.app, proposalID, approval.ChannelSlack)
		},
		"insert after cancel": func() (bool, error) {
			return store.InsertPosted(ctx, f.app, proposalID, approval.ChannelSlack,
				"C0123456789:1732041600.999999")
		},
	} {
		changed, err := transition()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changed {
			t.Fatalf("%s changed=true, want idempotent no-op", name)
		}
	}
	message, _, _ = store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if message.State != slack.StateCancelled || !message.UpdatedAt.Equal(cancelledAt) {
		t.Fatalf("terminal message mutated: %+v", message)
	}

	// Missing rows are no-ops, not errors.
	for name, transition := range map[string]func() (bool, error){
		"update": func() (bool, error) {
			return store.MarkUpdated(ctx, f.app, proposalID, "missing")
		},
		"cancel": func() (bool, error) {
			return store.MarkCancelled(ctx, f.app, proposalID, "missing")
		},
	} {
		changed, err := transition()
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if changed {
			t.Fatalf("missing %s changed=true", name)
		}
	}
}

func TestStoreAcceptsTransactionQueryer(t *testing.T) {
	f := newStoreFixture(t)
	store := &slack.Store{}
	ctx := context.Background()
	proposalID := f.newProposal()

	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.InsertPosted(ctx, tx, proposalID, approval.ChannelSlack,
			"C0123456789:1732041600.123456")
		return err
	})
	if err != nil {
		t.Fatalf("InsertPosted in tx: %v", err)
	}
	message, found, err := store.Get(ctx, f.app, proposalID, approval.ChannelSlack)
	if err != nil || !found {
		t.Fatalf("Get after tx: found=%t err=%v", found, err)
	}
	if message.State != slack.StatePosted {
		t.Fatalf("state after tx = %q, want posted", message.State)
	}
}
