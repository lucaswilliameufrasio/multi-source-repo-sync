package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/workflow"
)

// The access code of the test server, made the way the command makes it.
var accessCode = func() string {
	value, err := NewToken()
	if err != nil {
		panic(err)
	}
	return value
}()

type reader struct{ heads map[string]map[string]string }

func (r reader) ReadRepository(_ context.Context, owner string, repository string, _ string) (github.Snapshot, error) {
	branches := r.heads[owner+"/"+repository]
	copied := map[string]string{}
	for name, sha := range branches {
		copied[name] = sha
	}
	return github.Snapshot{Branches: copied}, nil
}

type inspector struct{ comparison gitinspect.Comparison }

func (i inspector) Compare(_ context.Context, _ string, _ string, heads []gitinspect.BranchHeads) (map[string]gitinspect.Comparison, error) {
	out := map[string]gitinspect.Comparison{}
	for _, head := range heads {
		out[head.Name] = i.comparison
	}
	return out, nil
}

type writes struct {
	mu       sync.Mutex
	pushes   int
	prs      int
	branches []string
	entered  chan struct{}
	release  chan struct{}
}

func (w *writes) PushCandidate(_ context.Context, _ string, _ string, _ string, candidate string, _ string, _ string) (gitinspect.Candidate, error) {
	w.mu.Lock()
	w.pushes++
	w.mu.Unlock()
	if w.entered != nil {
		w.entered <- struct{}{}
		<-w.release
	}
	return gitinspect.Candidate{BranchName: candidate, CommitSHA: strings.Repeat("c", 40)}, nil
}

func (w *writes) EnsurePullRequest(_ context.Context, owner string, repository string, _ string, _ string, _ string, _ string) (github.PullRequestResult, error) {
	w.mu.Lock()
	w.prs++
	w.mu.Unlock()
	return github.PullRequestResult{URL: "https://github.com/" + owner + "/" + repository + "/pull/7", Created: true}, nil
}

func (w *writes) PushBranch(_ context.Context, targetURL string, _ string, branch string, _ string, _ string) (gitinspect.BranchPush, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pushes++
	w.branches = append(w.branches, targetURL+" "+branch)
	return gitinspect.BranchPush{BranchName: branch, Ahead: 2}, nil
}

func (w *writes) MergeCommitAllowed(context.Context, string, string) (bool, error) { return true, nil }

func manifest() config.Config {
	return config.Config{
		Version: 1,
		Organizations: config.Organizations{
			{Label: "acme", Owner: "Acme-Org"},
			{Label: "globex", Owner: "Globex-Org"},
		},
		Services:             []string{"api-gateway"},
		SyncBranches:         []string{"main"},
		PreserveOnlyBranches: []string{"dev-test"},
	}
}

func fixture(t *testing.T, comparison gitinspect.Comparison) (*httptest.Server, *writes) {
	t.Helper()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	w := &writes{}
	flow := workflow.Workflow{
		Reader:    reader{heads: map[string]map[string]string{"Acme-Org/api-gateway": {"main": sha("a")}, "Globex-Org/api-gateway": {"main": sha("b")}}},
		Inspector: inspector{comparison: comparison},
		Git:       w, PRs: w, Merge: w,
	}
	server := httptest.NewServer(New(manifest(), flow, accessCode).Handler())
	t.Cleanup(server.Close)
	return server, w
}

func do(t *testing.T, server *httptest.Server, method string, path string, body any, mutate func(*http.Request)) (*http.Response, string) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.AddCookie(&http.Cookie{Name: cookieName, Value: accessCode})
	if mutate != nil {
		mutate(request)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(response.Body)
	return response, out.String()
}

func TestNothingIsServedWithoutTheTokenAndTheTokenBecomesACookie(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, gitinspect.Comparison{})
	noCookie := func(r *http.Request) { r.Header.Del("Cookie") }

	for _, path := range []string{"/", "/api/manifest", "/api/report"} {
		response, _ := do(t, server, "GET", path, nil, noCookie)
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a cookie = %d, want 401", path, response.StatusCode)
		}
	}
	if response, _ := do(t, server, "GET", "/?token=wrong", nil, noCookie); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token = %d, want 401", response.StatusCode)
	}
	response, _ := do(t, server, "GET", "/?token="+accessCode, nil, noCookie)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatalf("the right token = %d %q, want a redirect to /", response.StatusCode, response.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range response.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want HttpOnly and SameSite=Strict", cookie)
	}
	// The token cannot be used on the API itself (it would end up in logs and history).
	if response, _ := do(t, server, "GET", "/api/manifest?token="+accessCode, nil, noCookie); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token in an API URL = %d, want 401", response.StatusCode)
	}
}

func TestAnotherHostOrAnotherOriginIsRefusedEvenWithTheCookie(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, gitinspect.Comparison{})
	rebinding := func(r *http.Request) { r.Host = "evil.example.com" }
	if response, _ := do(t, server, "GET", "/api/manifest", nil, rebinding); response.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host = %d, want 403 (DNS rebinding)", response.StatusCode)
	}
	foreign := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.com") }
	body := map[string]string{"service": "api-gateway", "branch": "main", "pair": "acme/globex"}
	if response, _ := do(t, server, "POST", "/api/sync/propose", body, foreign); response.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Origin = %d, want 403 (CSRF)", response.StatusCode)
	}
	if response, _ := do(t, server, "GET", "/api/manifest", nil, nil); response.StatusCode != http.StatusOK {
		t.Errorf("the right Host with the cookie = %d, want 200", response.StatusCode)
	}
}

func TestPostsMustBeJSONAndKnownFieldsOnly(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, gitinspect.Comparison{})
	form := func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }
	if response, _ := do(t, server, "POST", "/api/sync/propose", map[string]string{"service": "api-gateway"}, form); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a form post = %d, want 415", response.StatusCode)
	}
	response, _ := do(t, server, "POST", "/api/sync/propose", map[string]string{"service": "api-gateway", "branch": "main", "pair": "acme/globex", "sneaky": "x"}, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400", response.StatusCode)
	}
}

func TestTheManifestAndTheReportAreServed(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, gitinspect.Comparison{})
	response, body := do(t, server, "GET", "/api/manifest", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"pairs":["acme/globex"]`) {
		t.Fatalf("manifest = %d %s", response.StatusCode, body)
	}
	response, body = do(t, server, "GET", "/api/report?mode=status", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("report = %d %s", response.StatusCode, body)
	}
	var report domain.Report
	if err := json.Unmarshal([]byte(body), &report); err != nil || len(report.Repositories) != 1 || report.Repositories[0].Pair != "acme/globex" {
		t.Fatalf("report = %s (%v)", body, err)
	}
	if response, _ := do(t, server, "GET", "/api/report?mode=nope", nil, nil); response.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown mode = %d, want 400", response.StatusCode)
	}
	if response, _ := do(t, server, "GET", "/api/report?service=api-gateway&pair=x/y", nil, nil); response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an unknown pair = %d, want 422", response.StatusCode)
	}
}

func TestThePageIsServedWithAStrictPolicy(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, gitinspect.Comparison{})
	response, body := do(t, server, "GET", "/", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Multi-Source Repo Sync") {
		t.Fatalf("page = %d", response.StatusCode)
	}
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP = %q", csp)
	}
	for _, header := range []string{"X-Frame-Options", "X-Content-Type-Options", "Cache-Control"} {
		if response.Header.Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
	if strings.Contains(body, "innerHTML") {
		t.Error("the page must not build markup from data")
	}
}

func TestSyncProposesThenWritesOnlyWithTheExactPhraseAndTheReviewedHeads(t *testing.T) {
	t.Parallel()
	server, w := fixture(t, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	sha := func(c string) string { return strings.Repeat(c, 40) }
	body := map[string]string{"service": "api-gateway", "branch": "main", "pair": "acme/globex"}

	response, text := do(t, server, "POST", "/api/sync/propose", body, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("propose = %d %s", response.StatusCode, text)
	}
	var proposal workflow.Proposal
	if err := json.Unmarshal([]byte(text), &proposal); err != nil || len(proposal.Writes) != 1 || proposal.Phrase != "sync api-gateway main acme/globex" {
		t.Fatalf("proposal = %s (%v)", text, err)
	}
	if w.pushes+w.prs != 0 {
		t.Fatal("a proposal must write nothing")
	}

	apply := func(phrase string, left string, right string) (*http.Response, string) {
		return do(t, server, "POST", "/api/sync/apply", map[string]string{
			"service": "api-gateway", "branch": "main", "pair": "acme/globex", "confirmation": phrase, "left_sha": left, "right_sha": right,
		}, nil)
	}
	if response, text := apply("sync api-gateway main", sha("a"), sha("b")); response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(text, "confirmation_mismatch") {
		t.Fatalf("a wrong phrase = %d %s", response.StatusCode, text)
	}
	if response, text := apply(proposal.Phrase, sha("a"), sha("z")); response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(text, "plan_changed") {
		t.Fatalf("heads that moved = %d %s", response.StatusCode, text)
	}
	if w.pushes+w.prs != 0 {
		t.Fatal("neither refusal may write")
	}
	response, text = apply(proposal.Phrase, sha("a"), sha("b"))
	if response.StatusCode != http.StatusOK || !strings.Contains(text, "https://github.com/Acme-Org/api-gateway/pull/7") {
		t.Fatalf("apply = %d %s", response.StatusCode, text)
	}
	if w.pushes != 1 || w.prs != 1 {
		t.Fatalf("writes = %d pushes, %d pull requests, want one of each", w.pushes, w.prs)
	}
}

func TestTwoSyncsAtTheSameTimeAreNotRun(t *testing.T) {
	t.Parallel()
	server, w := fixture(t, gitinspect.Comparison{Relation: gitinspect.RelationLeftBehind})
	w.entered = make(chan struct{})
	w.release = make(chan struct{})
	sha := func(c string) string { return strings.Repeat(c, 40) }
	apply := map[string]string{
		"service": "api-gateway", "branch": "main", "pair": "acme/globex",
		"confirmation": "sync api-gateway main acme/globex", "left_sha": sha("a"), "right_sha": sha("b"),
	}
	done := make(chan int)
	go func() {
		response, _ := do(t, server, "POST", "/api/sync/apply", apply, nil)
		done <- response.StatusCode
	}()
	<-w.entered // the first one is in the middle of its push
	response, text := do(t, server, "POST", "/api/sync/apply", apply, nil)
	if response.StatusCode != http.StatusConflict || !strings.Contains(text, "busy") {
		t.Fatalf("the second apply = %d %s, want 409 busy", response.StatusCode, text)
	}
	close(w.release)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("the first apply = %d, want 200", status)
	}
}

func TestOnlyLoopbackAddressesAreAccepted(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"127.0.0.1:7878", "[::1]:7878", "localhost:7878", "127.0.0.1:0"} {
		if err := RequireLoopback(ok); err != nil {
			t.Errorf("RequireLoopback(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:7878", "192.168.1.5:7878", "[::]:7878", "example.com:80", ":7878", "7878"} {
		if err := RequireLoopback(bad); err == nil {
			t.Errorf("RequireLoopback(%q) accepted an address other machines can reach", bad)
		}
	}
}

func propagationFixture(t *testing.T) (*httptest.Server, *writes) {
	t.Helper()
	sha := func(c string) string { return strings.Repeat(c, 40) }
	w := &writes{}
	flow := workflow.Workflow{
		Reader: reader{heads: map[string]map[string]string{
			"Acme-Org/api-gateway":   {"main": sha("a"), "feat/edge": sha("f"), "dev-test": sha("d"), "sync/multi-source-repo-sync/x/main/1-2": sha("e")},
			"Globex-Org/api-gateway": {"main": sha("b")},
		}},
		Inspector: inspector{}, Git: w, PRs: w, Merge: w, Branches: w,
	}
	server := httptest.NewServer(New(manifest(), flow, accessCode).Handler())
	t.Cleanup(server.Close)
	return server, w
}

func TestOnlyBranchesThatCanBePropagatedAreListed(t *testing.T) {
	t.Parallel()
	server, _ := propagationFixture(t)
	response, body := do(t, server, "GET", "/api/branches?service=api-gateway&org=acme", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `"branches":["feat/edge"]`) {
		t.Fatalf("branches = %d %s, want only feat/edge (not main, dev-test or a candidate)", response.StatusCode, body)
	}
	if response, _ := do(t, server, "GET", "/api/branches?service=api-gateway&org=nobody", nil, nil); response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an unknown organization = %d, want 422", response.StatusCode)
	}
}

func TestPropagationNeedsTheExactPhraseAndTheReviewedCommit(t *testing.T) {
	t.Parallel()
	server, w := propagationFixture(t)
	sha := func(c string) string { return strings.Repeat(c, 40) }

	response, text := do(t, server, "POST", "/api/propagate/propose", map[string]any{"service": "api-gateway", "branch": "feat/edge", "source": "acme", "targets": []string{}}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("propose = %d %s", response.StatusCode, text)
	}
	var proposal workflow.PropagateProposal
	if err := json.Unmarshal([]byte(text), &proposal); err != nil || len(proposal.Targets) != 1 || proposal.Targets[0].State != workflow.StateNew {
		t.Fatalf("proposal = %s (%v)", text, err)
	}
	apply := func(phrase string, source string) (*http.Response, string) {
		return do(t, server, "POST", "/api/propagate/apply", map[string]any{
			"service": "api-gateway", "branch": "feat/edge", "source": "acme", "targets": []string{"globex"}, "base": "main",
			"confirmation": phrase, "source_sha": source,
		}, nil)
	}
	if response, text := apply("propagate api-gateway feat/edge from acme", sha("f")); response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(text, "confirmation_mismatch") {
		t.Fatalf("a wrong phrase = %d %s", response.StatusCode, text)
	}
	if response, text := apply(proposal.Phrase, sha("0")); response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(text, "plan_changed") {
		t.Fatalf("a moved source = %d %s", response.StatusCode, text)
	}
	if w.pushes+w.prs != 0 {
		t.Fatal("neither refusal may write")
	}
	response, text = apply(proposal.Phrase, sha("f"))
	if response.StatusCode != http.StatusOK || !strings.Contains(text, `"status":"created"`) || !strings.Contains(text, "https://github.com/Globex-Org/api-gateway/pull/7") {
		t.Fatalf("apply = %d %s", response.StatusCode, text)
	}
	if len(w.branches) != 1 || !strings.Contains(w.branches[0], "Globex-Org/api-gateway.git feat/edge") || w.prs != 1 {
		t.Fatalf("writes = %v, %d pull requests", w.branches, w.prs)
	}
}
