package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const protectedPreserveOnlyBranch = "dev-test"

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var safeLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Organization is one place where the services live: a short label used in commands and branch
// names, and the GitHub owner it stands for.
type Organization struct {
	Label string
	Owner string
}

// Organizations keeps the order in which the manifest lists them, because the first of a pair is
// its left side.
type Organizations []Organization

func (o *Organizations) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("organizations must be an object of label: owner")
	}
	var list Organizations
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		label, _ := key.(string)
		var owner string
		if err := decoder.Decode(&owner); err != nil {
			return fmt.Errorf("organizations.%s must be a GitHub owner name: %w", label, err)
		}
		list = append(list, Organization{Label: label, Owner: owner})
	}
	*o = list
	return nil
}

func (o Organizations) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for index, organization := range o {
		if index > 0 {
			out.WriteByte(',')
		}
		label, _ := json.Marshal(organization.Label)
		owner, _ := json.Marshal(organization.Owner)
		out.Write(label)
		out.WriteByte(':')
		out.Write(owner)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// Find returns the organization with this label.
func (o Organizations) Find(label string) (Organization, bool) {
	for _, organization := range o {
		if organization.Label == label {
			return organization, true
		}
	}
	return Organization{}, false
}

// Pair is the unit of synchronization: two organizations, a left and a right side.
type Pair struct {
	Left  Organization
	Right Organization
}

// Name is how a pair is written on the command line: "left/right".
func (p Pair) Name() string { return p.Left.Label + "/" + p.Right.Label }

type Config struct {
	Version       int           `json:"version"`
	Organizations Organizations `json:"organizations"`
	PairLabels    [][]string    `json:"pairs,omitempty"`
	// OrganizationServices says which services an organization has when it does not have all of
	// them (label -> service names). An organization that is not listed has every service.
	OrganizationServices map[string][]string `json:"organization_services,omitempty"`
	Services             []string            `json:"services"`
	SyncBranches         []string            `json:"sync_branches"`
	PreserveOnlyBranches []string            `json:"preserve_only_branches"`
}

// Pairs lists the pairs to synchronize: the ones the manifest names, or every pair of
// organizations in the order they are listed. The configuration must be valid.
func (c Config) Pairs() []Pair {
	var pairs []Pair
	if len(c.PairLabels) > 0 {
		for _, labels := range c.PairLabels {
			left, _ := c.Organizations.Find(labels[0])
			right, _ := c.Organizations.Find(labels[1])
			pairs = append(pairs, Pair{Left: left, Right: right})
		}
		return pairs
	}
	for i := range c.Organizations {
		for j := i + 1; j < len(c.Organizations); j++ {
			pairs = append(pairs, Pair{Left: c.Organizations[i], Right: c.Organizations[j]})
		}
	}
	return pairs
}

// HasService says whether the organization with this label keeps a copy of the service.
func (c Config) HasService(label string, service string) bool {
	only, limited := c.OrganizationServices[label]
	if !limited {
		return true
	}
	for _, name := range only {
		if name == service {
			return true
		}
	}
	return false
}

// PairsFor lists the pairs that have a copy of the service on both sides.
func (c Config) PairsFor(service string) []Pair {
	var pairs []Pair
	for _, pair := range c.Pairs() {
		if c.HasService(pair.Left.Label, service) && c.HasService(pair.Right.Label, service) {
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// Pair finds a pair by its "left/right" name (either order).
func (c Config) Pair(name string) (Pair, bool) {
	for _, pair := range c.Pairs() {
		if pair.Name() == name {
			return pair, true
		}
		if pair.Right.Label+"/"+pair.Left.Label == name {
			return pair, true
		}
	}
	return Pair{}, false
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	contents, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return Config{}, fmt.Errorf("read config: %w", readErr)
	}
	if closeErr != nil {
		return Config{}, fmt.Errorf("close config: %w", closeErr)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode config: unexpected content after JSON document")
		}
		return Config{}, fmt.Errorf("decode config trailing content: %w", err)
	}

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func Validate(cfg Config) error {
	if cfg.Version != 1 {
		return fmt.Errorf("unsupported config version %d; expected 1", cfg.Version)
	}
	if err := validateOrganizations(cfg); err != nil {
		return err
	}
	if err := validatePairs(cfg); err != nil {
		return err
	}
	if err := validateOrganizationServices(cfg); err != nil {
		return err
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("services must contain at least one repository")
	}
	if len(cfg.SyncBranches) == 0 {
		return fmt.Errorf("sync_branches must contain at least one branch")
	}
	if err := validateUniqueNames("services", cfg.Services); err != nil {
		return err
	}
	if err := validateUniqueNames("sync_branches", cfg.SyncBranches); err != nil {
		return err
	}
	if err := validateUniqueNames("preserve_only_branches", cfg.PreserveOnlyBranches); err != nil {
		return err
	}

	for _, branch := range cfg.SyncBranches {
		if branch == protectedPreserveOnlyBranch {
			return fmt.Errorf("%q is preserve-only and cannot appear in sync_branches", protectedPreserveOnlyBranch)
		}
	}

	preserveOnly := make(map[string]struct{}, len(cfg.PreserveOnlyBranches))
	for _, branch := range cfg.PreserveOnlyBranches {
		if !safeName.MatchString(branch) {
			return fmt.Errorf("preserve_only_branches contains an invalid branch name")
		}
		preserveOnly[branch] = struct{}{}
	}
	if _, ok := preserveOnly[protectedPreserveOnlyBranch]; !ok {
		return fmt.Errorf("preserve_only_branches must include %q", protectedPreserveOnlyBranch)
	}

	for _, branch := range cfg.SyncBranches {
		if !safeName.MatchString(branch) {
			return fmt.Errorf("sync_branches contains an invalid branch name")
		}
		if _, ok := preserveOnly[branch]; ok {
			return fmt.Errorf("branch %q cannot be both syncable and preserve-only", branch)
		}
	}

	for _, service := range cfg.Services {
		if !safeName.MatchString(service) {
			return fmt.Errorf("services contains an invalid repository name")
		}
	}

	return nil
}

func validateUniqueNames(field string, names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s contains duplicate value %q", field, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateOrganizations(cfg Config) error {
	if len(cfg.Organizations) < 2 {
		return fmt.Errorf("organizations must name at least two")
	}
	labels := make(map[string]struct{}, len(cfg.Organizations))
	owners := make(map[string]struct{}, len(cfg.Organizations))
	for _, organization := range cfg.Organizations {
		if !safeLabel.MatchString(organization.Label) {
			return fmt.Errorf("organizations label %q must be lowercase letters, digits or -", organization.Label)
		}
		if !safeName.MatchString(organization.Owner) {
			return fmt.Errorf("organizations.%s must be a simple GitHub owner name", organization.Label)
		}
		if _, ok := labels[organization.Label]; ok {
			return fmt.Errorf("organizations has the label %q twice", organization.Label)
		}
		labels[organization.Label] = struct{}{}
		lowerOwner := strings.ToLower(organization.Owner)
		if _, ok := owners[lowerOwner]; ok {
			return fmt.Errorf("organizations names the owner %q twice; each label needs a different owner", organization.Owner)
		}
		owners[lowerOwner] = struct{}{}
	}
	return nil
}

func validatePairs(cfg Config) error {
	seen := make(map[string]struct{}, len(cfg.PairLabels))
	for _, labels := range cfg.PairLabels {
		if len(labels) != 2 {
			return fmt.Errorf("each entry of pairs must be two organization labels")
		}
		for _, label := range labels {
			if _, ok := cfg.Organizations.Find(label); !ok {
				return fmt.Errorf("pairs mentions %q, which is not in organizations", label)
			}
		}
		if labels[0] == labels[1] {
			return fmt.Errorf("a pair needs two different organizations, not %q twice", labels[0])
		}
		key := labels[0] + "/" + labels[1]
		reverse := labels[1] + "/" + labels[0]
		if _, ok := seen[key]; ok {
			return fmt.Errorf("pairs lists %s twice", key)
		}
		if _, ok := seen[reverse]; ok {
			return fmt.Errorf("pairs lists %s twice (in both orders)", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateOrganizationServices(cfg Config) error {
	known := make(map[string]struct{}, len(cfg.Services))
	for _, service := range cfg.Services {
		known[service] = struct{}{}
	}
	for label, services := range cfg.OrganizationServices {
		if _, ok := cfg.Organizations.Find(label); !ok {
			return fmt.Errorf("organization_services mentions %q, which is not in organizations", label)
		}
		if len(services) == 0 {
			return fmt.Errorf("organization_services.%s is empty; leave the organization out to give it every service", label)
		}
		if err := validateUniqueNames("organization_services."+label, services); err != nil {
			return err
		}
		for _, service := range services {
			if _, ok := known[service]; !ok {
				return fmt.Errorf("organization_services.%s names %q, which is not in services", label, service)
			}
		}
	}
	return nil
}
