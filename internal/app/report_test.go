package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/github"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/gitinspect"
)

type fakeReader struct {
	snapshots map[string]github.Snapshot
	errors    map[string]error
}

func (r fakeReader) ReadRepository(_ context.Context, owner string, repository string, _ string) (github.Snapshot, error) {
	key := owner + "/" + repository
	if err := r.errors[key]; err != nil {
		return github.Snapshot{}, err
	}
	snapshot, ok := r.snapshots[key]
	if !ok {
		return github.Snapshot{}, errors.New("repository is unavailable")
	}
	return snapshot, nil
}

type fakeInspector struct {
	seenBranches []string
	comparisons  map[string]gitinspect.Comparison
	err          error
}

func (i *fakeInspector) Compare(_ context.Context, _ string, _ string, heads []gitinspect.BranchHeads) (map[string]gitinspect.Comparison, error) {
	for _, head := range heads {
		i.seenBranches = append(i.seenBranches, head.Name)
	}
	return i.comparisons, i.err
}

func TestPlanPreservesDevTestAndOnlyInspectsConfiguredBranches(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	reader := fakeReader{snapshots: map[string]github.Snapshot{
		"Acme-Org/api-gateway": {
			Branches: map[string]string{
				"main":             "same-main",
				"develop":          "tix-develop",
				"dev-test":         "tix-dev-test",
				"feature/only-tix": "feature-sha",
			},
			OpenPullRequests: []domain.PullRequest{{Side: "acme", Number: 31, HeadBranch: "bugfix/security", BaseBranch: "develop"}},
		},
		"Globex-Org/api-gateway": {
			Branches: map[string]string{
				"main":     "same-main",
				"develop":  "globex-develop",
				"dev-test": "globex-dev-test",
			},
		},
	}}
	inspector := &fakeInspector{comparisons: map[string]gitinspect.Comparison{
		"develop": {Relation: gitinspect.RelationDiverged},
	}}
	reporter := Reporter{Reader: reader, Inspector: inspector}

	report, err := reporter.Build(context.Background(), cfg, modePlan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(report.Repositories) != 1 {
		t.Fatalf("got %d repositories, want 1", len(report.Repositories))
	}
	if !reflect.DeepEqual(inspector.seenBranches, []string{"develop"}) {
		t.Fatalf("inspected branches = %#v, want only develop", inspector.seenBranches)
	}

	branches := make(map[string]domain.BranchStatus)
	for _, branch := range report.Repositories[0].Branches {
		branches[branch.Name] = branch
	}
	devTest := branches["dev-test"]
	if devTest.Policy != "preserve_only" || devTest.Assessment != "preserved" || devTest.Action != "never_sync_or_delete" {
		t.Fatalf("dev-test status = %+v, want preserve-only", devTest)
	}
	if got, want := branches["develop"].Assessment, "clean_merge"; got != want {
		t.Fatalf("develop assessment = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(branches["develop"].PRTargets, []string{"acme", "globex"}) {
		t.Fatalf("develop PR targets = %#v, want both sides", branches["develop"].PRTargets)
	}
	if branches["feature/only-tix"].Policy != "inventory_only" || branches["feature/only-tix"].Assessment != "missing_on_globex" {
		t.Fatalf("feature branch status = %+v, want inventory-only and preserved", branches["feature/only-tix"])
	}
	if len(report.Repositories[0].Left.OpenPullRequests) != 1 {
		t.Fatalf("got %d Acme PRs, want 1", len(report.Repositories[0].Left.OpenPullRequests))
	}
}

func TestPlanMarksHistoryInspectionFailureAsStop(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	reader := pairedReader(map[string]string{"main": "same", "develop": "left"}, map[string]string{"main": "same", "develop": "right"})
	inspector := &fakeInspector{err: errors.New("Git fetch failed with exit code 128")}
	reporter := Reporter{Reader: reader, Inspector: inspector}

	report, err := reporter.Build(context.Background(), cfg, modePlan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	var develop domain.BranchStatus
	for _, branch := range report.Repositories[0].Branches {
		if branch.Name == "develop" {
			develop = branch
		}
	}
	if develop.Assessment != "history_unavailable" || develop.Action != "stop_and_retry" {
		t.Fatalf("develop status = %+v, want history-unavailable stop", develop)
	}
	if len(report.Repositories[0].Errors) != 1 {
		t.Fatalf("repository errors = %#v, want one history error", report.Repositories[0].Errors)
	}
}

func TestStatusDoesNotAttemptHistoryInspection(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	reader := pairedReader(map[string]string{"main": "left", "develop": "left-dev", "dev-test": "left-test"}, map[string]string{"main": "right", "develop": "right-dev", "dev-test": "right-test"})
	reporter := Reporter{Reader: reader}

	report, err := reporter.Build(context.Background(), cfg, modeStatus)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, branch := range report.Repositories[0].Branches {
		if branch.Policy == "sync_candidate" && branch.Assessment == "different_heads" && branch.Action != "ancestry_not_checked" {
			t.Fatalf("status unexpectedly planned branch %q: %+v", branch.Name, branch)
		}
	}
}

func pairedReader(tixBranches map[string]string, globexBranches map[string]string) fakeReader {
	return fakeReader{snapshots: map[string]github.Snapshot{
		"Acme-Org/api-gateway":   {Branches: tixBranches},
		"Globex-Org/api-gateway": {Branches: globexBranches},
	}}
}

func testConfig() config.Config {
	return config.Config{
		Version: 1,
		Organizations: config.Organizations{
			{Label: "acme", Owner: "Acme-Org"},
			{Label: "globex", Owner: "Globex-Org"},
		},
		Services:             []string{"api-gateway"},
		SyncBranches:         []string{"main", "develop"},
		PreserveOnlyBranches: []string{"dev-test"},
	}
}

type countingReader struct {
	fakeReader
	calls map[string]int
}

func (r *countingReader) ReadRepository(ctx context.Context, owner string, repository string, side string) (github.Snapshot, error) {
	r.calls[owner+"/"+repository]++
	return r.fakeReader.ReadRepository(ctx, owner, repository, side)
}

func threeOrganizationConfig() config.Config {
	cfg := testConfig()
	cfg.Version = 1
	cfg.Organizations = config.Organizations{
		{Label: "initech", Owner: "Initech-Org"},
		{Label: "acme", Owner: "Acme-Org"},
		{Label: "globex", Owner: "Globex-Org"},
	}
	cfg.Services = []string{"api-gateway", "ledger-service"}
	cfg.OrganizationServices = map[string][]string{"initech": {"api-gateway"}}
	return cfg
}

func TestThreeOrganizationsGiveOneReportEntryPerServiceAndPair(t *testing.T) {
	t.Parallel()

	reader := &countingReader{calls: map[string]int{}, fakeReader: fakeReader{snapshots: map[string]github.Snapshot{
		"Initech-Org/api-gateway":   {Branches: map[string]string{"main": "euf"}},
		"Acme-Org/api-gateway":      {Branches: map[string]string{"main": "tix"}},
		"Globex-Org/api-gateway":    {Branches: map[string]string{"main": "euf"}},
		"Acme-Org/ledger-service":   {Branches: map[string]string{"main": "same"}},
		"Globex-Org/ledger-service": {Branches: map[string]string{"main": "same"}},
	}}}
	report, err := Reporter{Reader: reader}.Build(context.Background(), threeOrganizationConfig(), modeStatus)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	var got []string
	for _, repository := range report.Repositories {
		got = append(got, repository.Service+" "+repository.Pair)
	}
	want := []string{
		"api-gateway initech/acme", "api-gateway initech/globex", "api-gateway acme/globex",
		"ledger-service acme/globex",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v (initech has no ledger-service)", got, want)
	}
	for repository, count := range reader.calls {
		if count != 1 {
			t.Errorf("%s was read %d times, want once however many pairs it is in", repository, count)
		}
	}

	assessments := map[string]string{}
	for _, repository := range report.Repositories {
		for _, branch := range repository.Branches {
			if branch.Name == "main" {
				assessments[repository.Service+" "+repository.Pair] = branch.Assessment
			}
		}
	}
	if assessments["api-gateway initech/globex"] != "in_sync" ||
		assessments["api-gateway initech/acme"] != "different_heads" ||
		assessments["api-gateway acme/globex"] != "different_heads" ||
		assessments["ledger-service acme/globex"] != "in_sync" {
		t.Fatalf("assessments = %v", assessments)
	}
}

func TestAPairNamesTheOrganizationThatReceivesThePullRequest(t *testing.T) {
	t.Parallel()

	cfg := threeOrganizationConfig()
	reader := fakeReader{snapshots: map[string]github.Snapshot{
		"Initech-Org/api-gateway": {Branches: map[string]string{"main": "euf"}},
		"Acme-Org/api-gateway":    {Branches: map[string]string{"main": "tix"}},
		"Globex-Org/api-gateway":  {Branches: map[string]string{"main": "tix"}},
	}}
	inspector := &fakeInspector{comparisons: map[string]gitinspect.Comparison{"main": {Relation: gitinspect.RelationLeftBehind}}}
	cfg.Services = []string{"api-gateway"}
	cfg.PairLabels = [][]string{{"initech", "acme"}}
	report, err := Reporter{Reader: reader, Inspector: inspector}.Build(context.Background(), cfg, modePlan)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(report.Repositories) != 1 {
		t.Fatalf("entries = %d, want 1 (pairs restricted in the manifest)", len(report.Repositories))
	}
	var main domain.BranchStatus
	for _, branch := range report.Repositories[0].Branches {
		if branch.Name == "main" {
			main = branch
		}
	}
	if !reflect.DeepEqual(main.PRTargets, []string{"initech"}) {
		t.Fatalf("PR targets = %v, want the left side (initech) when it is behind", main.PRTargets)
	}
}
