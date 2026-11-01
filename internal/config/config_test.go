package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version: 1,
		Organizations: Organizations{
			{Label: "acme", Owner: "Acme-Org"},
			{Label: "globex", Owner: "Globex-Org"},
		},
		Services:             []string{"api-gateway"},
		SyncBranches:         []string{"main", "develop"},
		PreserveOnlyBranches: []string{"dev-test"},
	}
}

func TestValidateRequiresDevTestPreserveOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "dev-test cannot be synchronized",
			mutate: func(cfg *Config) {
				cfg.SyncBranches = append(cfg.SyncBranches, "dev-test")
			},
			want: "preserve-only",
		},
		{
			name: "dev-test must be preserved",
			mutate: func(cfg *Config) {
				cfg.PreserveOnlyBranches = nil
			},
			want: `must include "dev-test"`,
		},
		{
			name: "branch cannot be both policies",
			mutate: func(cfg *Config) {
				cfg.PreserveOnlyBranches = append(cfg.PreserveOnlyBranches, "develop")
			},
			want: "both syncable and preserve-only",
		},
		{
			name: "duplicate service is rejected",
			mutate: func(cfg *Config) {
				cfg.Services = append(cfg.Services, "api-gateway")
			},
			want: "duplicate value",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			testCase.mutate(&cfg)
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestTheExampleManifestIsValidAndHasTheExpectedScope(t *testing.T) {
	t.Parallel()

	cfg, err := Load(filepath.Join("..", "..", "config", "repos.example.json"))
	if err != nil {
		t.Fatalf("Load(default manifest) error = %v", err)
	}
	wantServices := []string{"api-gateway", "auth-service", "worker-service"}
	if !reflect.DeepEqual(cfg.Services, wantServices) {
		t.Fatalf("services = %#v, want %#v", cfg.Services, wantServices)
	}
	if !reflect.DeepEqual(cfg.SyncBranches, []string{"main", "develop"}) {
		t.Fatalf("sync branches = %#v, want main and develop", cfg.SyncBranches)
	}
	if !reflect.DeepEqual(cfg.PreserveOnlyBranches, []string{"dev-test"}) {
		t.Fatalf("preserve-only branches = %#v, want dev-test", cfg.PreserveOnlyBranches)
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown field",
			body: `{"version":1,"organizations":{"acme":"Acme-Org","globex":"Globex-Org"},"services":["api-gateway"],"sync_branches":["main"],"preserve_only_branches":["dev-test"],"token":"not-allowed"}`,
			want: "unknown field",
		},
		{
			name: "trailing document",
			body: `{"version":1,"organizations":{"acme":"Acme-Org","globex":"Globex-Org"},"services":["api-gateway"],"sync_branches":["main"],"preserve_only_branches":["dev-test"]} {}`,
			want: "unexpected content",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "repos.json")
			if err := os.WriteFile(path, []byte(testCase.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

func threeOrganizations() Config {
	cfg := validConfig()
	cfg.Version = 1
	cfg.Organizations = Organizations{
		{Label: "initech", Owner: "Initech-Org"},
		{Label: "acme", Owner: "Acme-Org"},
		{Label: "globex", Owner: "Globex-Org"},
	}
	return cfg
}

func pairNames(cfg Config) []string {
	var names []string
	for _, pair := range cfg.Pairs() {
		names = append(names, pair.Name())
	}
	return names
}

func TestEveryPairOfOrganizationsIsAPairByDefaultInManifestOrder(t *testing.T) {
	t.Parallel()

	cfg := threeOrganizations()
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	want := []string{"initech/acme", "initech/globex", "acme/globex"}
	if got := pairNames(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %v, want %v", got, want)
	}
	if got := pairNames(validConfig()); !reflect.DeepEqual(got, []string{"acme/globex"}) {
		t.Fatalf("version 1 pairs = %v, want acme/globex", got)
	}
}

func TestPairsCanBeRestrictedAndFoundInEitherOrder(t *testing.T) {
	t.Parallel()

	cfg := threeOrganizations()
	cfg.PairLabels = [][]string{{"initech", "acme"}, {"initech", "globex"}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := pairNames(cfg); !reflect.DeepEqual(got, []string{"initech/acme", "initech/globex"}) {
		t.Fatalf("pairs = %v", got)
	}
	pair, ok := cfg.Pair("globex/initech")
	if !ok || pair.Left.Owner != "Initech-Org" || pair.Right.Owner != "Globex-Org" {
		t.Fatalf("Pair(globex/initech) = %+v, %v; want the initech/globex pair", pair, ok)
	}
	if _, ok := cfg.Pair("acme/globex"); ok {
		t.Fatal("a pair left out of the manifest must not be found")
	}
}

func TestOrganizationsAreValidated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"one organization", func(c *Config) { c.Organizations = c.Organizations[:1] }, "at least two"},
		{"duplicate label", func(c *Config) { c.Organizations[1].Label = "initech" }, "twice"},
		{"same owner twice", func(c *Config) { c.Organizations[1].Owner = "INITECH-ORG" }, "twice"},
		{"label with capitals", func(c *Config) { c.Organizations[0].Label = "Initech" }, "lowercase"},
		{"owner with a slash", func(c *Config) { c.Organizations[0].Owner = "a/b" }, "simple GitHub owner"},
		{"pair of an unknown label", func(c *Config) { c.PairLabels = [][]string{{"initech", "nobody"}} }, "not in organizations"},
		{"pair with itself", func(c *Config) { c.PairLabels = [][]string{{"globex", "globex"}} }, "two different"},
		{"pair twice in both orders", func(c *Config) { c.PairLabels = [][]string{{"globex", "acme"}, {"acme", "globex"}} }, "both orders"},
		{"pair with three labels", func(c *Config) { c.PairLabels = [][]string{{"globex", "acme", "initech"}} }, "two organization labels"},
		{"unknown version", func(c *Config) { c.Version = 3 }, "unsupported config version"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cfg := threeOrganizations()
			testCase.mutate(&cfg)
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

func TestOrganizationsKeepTheOrderOfTheManifestWhenLoaded(t *testing.T) {
	t.Parallel()

	body := `{"version":1,"organizations":{"zeta":"Zeta-Org","alpha":"Alpha-Org","mid":"Mid-Org"},"services":["svc"],"sync_branches":["main"],"preserve_only_branches":["dev-test"]}`
	path := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := pairNames(cfg); !reflect.DeepEqual(got, []string{"zeta/alpha", "zeta/mid", "alpha/mid"}) {
		t.Fatalf("pairs = %v, want the order of the file", got)
	}
}

func TestAnOrganizationWithFewerServicesLeavesItsPairsOutForTheOthers(t *testing.T) {
	t.Parallel()

	cfg := threeOrganizations()
	cfg.Services = []string{"api-gateway", "ledger-service"}
	cfg.OrganizationServices = map[string][]string{"initech": {"api-gateway"}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	names := func(service string) []string {
		var out []string
		for _, pair := range cfg.PairsFor(service) {
			out = append(out, pair.Name())
		}
		return out
	}
	if got := names("api-gateway"); !reflect.DeepEqual(got, []string{"initech/acme", "initech/globex", "acme/globex"}) {
		t.Fatalf("api-gateway pairs = %v", got)
	}
	if got := names("ledger-service"); !reflect.DeepEqual(got, []string{"acme/globex"}) {
		t.Fatalf("ledger-service pairs = %v, want only acme/globex", got)
	}
}

func TestOrganizationServicesAreValidated(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		set  map[string][]string
		want string
	}{
		{"unknown organization", map[string][]string{"nobody": {"api-gateway"}}, "not in organizations"},
		{"unknown service", map[string][]string{"initech": {"missing"}}, "not in services"},
		{"empty list", map[string][]string{"initech": {}}, "is empty"},
		{"duplicate service", map[string][]string{"initech": {"api-gateway", "api-gateway"}}, "duplicate value"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cfg := threeOrganizations()
			cfg.OrganizationServices = testCase.set
			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, testCase.want)
			}
		})
	}
}
