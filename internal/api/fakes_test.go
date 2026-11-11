package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const (
	testAdminKey    = "admin-test-key-0123456789abcdef012345"
	testApproverKey = "approver-test-key-0123456789abcdef01"
	testViewerKey   = "viewer-test-key-0123456789abcdef0123"
)

var testClockTime = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func testKey(id string, role config.Role, secret string) config.APIKey {
	return config.APIKey{ID: id, Role: role, Digest: sha256.Sum256([]byte(secret))}
}

func testKeys() []config.APIKey {
	return []config.APIKey{
		testKey("dev-admin", config.RoleAdmin, testAdminKey),
		testKey("dev-approver", config.RoleApprover, testApproverKey),
		testKey("dev-viewer", config.RoleViewer, testViewerKey),
	}
}

type fakeIngest struct {
	result ingest.Result
	err    error
	got    ingest.Request
	calls  int
}

func (f *fakeIngest) Ingest(_ context.Context, req ingest.Request) (ingest.Result, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

type fakeMatcher struct {
	result match.RunResult
	err    error
	got    match.RunRequest
	calls  int
}

func (f *fakeMatcher) Run(_ context.Context, req match.RunRequest) (match.RunResult, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

type fakeCases struct {
	result cases.CreateBreaksResult
	err    error
	got    cases.CreateBreaksRequest
	calls  int
}

func (f *fakeCases) CreateBreaks(_ context.Context, req cases.CreateBreaksRequest) (cases.CreateBreaksResult, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

type fakeCaseReader struct {
	page  CaseListPage
	err   error
	got   CaseListQuery
	calls int
}

func (f *fakeCaseReader) ListCases(_ context.Context, query CaseListQuery) (CaseListPage, error) {
	f.calls++
	f.got = query
	return f.page, f.err
}

type fakeCaseFile struct {
	document CaseFile
	err      error
	got      string
	calls    int
}

func (f *fakeCaseFile) GetCaseFile(_ context.Context, caseID string) (CaseFile, error) {
	f.calls++
	f.got = caseID
	return f.document, f.err
}

type fakeProposalReader struct {
	page  ProposalListPage
	err   error
	got   ProposalListQuery
	calls int
}

func (f *fakeProposalReader) ListProposals(_ context.Context, query ProposalListQuery) (ProposalListPage, error) {
	f.calls++
	f.got = query
	return f.page, f.err
}

type fakeSwitches struct {
	result SwitchResult
	err    error
	got    SwitchSetRequest
	calls  int
}

func (f *fakeSwitches) SetSwitch(_ context.Context, req SwitchSetRequest) (SwitchResult, error) {
	f.calls++
	f.got = req
	return f.result, f.err
}

type harness struct {
	api       *API
	ingest    *fakeIngest
	matcher   *fakeMatcher
	cases     *fakeCases
	caseList  *fakeCaseReader
	caseFile  *fakeCaseFile
	proposals *fakeProposalReader
	switches  *fakeSwitches
	logs      *bytes.Buffer
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	logs := &bytes.Buffer{}
	opts := Options{
		Config:     config.DefaultAPIConfig(),
		Keys:       testKeys(),
		Logger:     telemetry.NewLogger(logs, slog.LevelDebug),
		Clock:      clock.NewFake(testClockTime),
		Ingest:     &fakeIngest{},
		Matcher:    &fakeMatcher{},
		Cases:      &fakeCases{},
		CaseReader: &fakeCaseReader{page: CaseListPage{Items: []CaseSummary{}}},
		CaseFile:   &fakeCaseFile{},
		Proposals:  &fakeProposalReader{page: ProposalListPage{Items: []ProposalSummary{}}},
		Switches:   &fakeSwitches{},
	}
	if mutate != nil {
		mutate(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &harness{api: a, logs: logs}
	h.ingest, _ = opts.Ingest.(*fakeIngest)
	h.matcher, _ = opts.Matcher.(*fakeMatcher)
	h.cases, _ = opts.Cases.(*fakeCases)
	h.caseList, _ = opts.CaseReader.(*fakeCaseReader)
	h.caseFile, _ = opts.CaseFile.(*fakeCaseFile)
	h.proposals, _ = opts.Proposals.(*fakeProposalReader)
	h.switches, _ = opts.Switches.(*fakeSwitches)
	return h
}

func (h *harness) request(method, target, body, contentType, bearer string, mutate func(*http.Request)) *http.Response {
	req, err := http.NewRequest(method, target, bytes.NewBufferString(body))
	if err != nil {
		panic(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if mutate != nil {
		mutate(req)
	}
	return serve(h.api, req)
}

func newTestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func serve(a *API, req *http.Request) *http.Response {
	recorder := newTestRecorder()
	a.Handler().ServeHTTP(recorder, req)
	return recorder.Result()
}

func (h *harness) expectStatus(method, target, body, contentType, bearer string, want int) *http.Response {
	return h.request(method, target, body, contentType, bearer, nil)
}

func bodyOf(t *testing.T, response *http.Response) string {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	buffer := new(bytes.Buffer)
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buffer.String()
}
