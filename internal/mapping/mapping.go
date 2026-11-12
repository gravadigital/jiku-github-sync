// Package mapping loads the connector's configuration file: which Jiku project maps to which
// GitHub repos and Project, and how the fields of each GitHub Project translate to Jiku.
//
// Nothing about a particular GitHub Project is compiled in. The only fixed side is Jiku's, whose
// states, priorities and types are enums of its contract. Every GitHub name (field, option, issue
// type, label) is configuration and is resolved to ids at runtime, by name, so a Project that
// renames or recreates its options keeps working once its names are mapped.
package mapping

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Jiku's enums (core contract). They are the only fixed vocabulary of the mapping.
var (
	JikuStates     = []string{"analisis", "planificacion", "en_cola", "desarrollo", "revision", "resuelto", "cancelado"}
	JikuPriorities = []string{"sin_prioridad", "baja", "media", "alta", "urgente"}
	JikuTypes      = []string{"funcionalidad", "mejora", "incidencia", "otro"}
)

// File is the whole configuration file.
type File struct {
	// Credentials are the GitHub credentials, one per client, by name. Without this section every
	// project uses the default credential (GITHUB_TOKEN).
	Credentials   map[string]*Credential   `yaml:"credentials"`
	FieldMappings map[string]*FieldMapping `yaml:"fieldMappings"`
	Projects      []*Project               `yaml:"projects"`
}

// DefaultCredential is the credential of projects that name none: GITHUB_TOKEN.
const DefaultCredential = "default"

// Credential is how the connector reaches a client's GitHub: a token, or the installations of
// the connector's GitHub App on the client's organizations. The secrets themselves never go in
// this file: it only says where to read them.
type Credential struct {
	Token *Secret        `yaml:"token"`
	App   *AppCredential `yaml:"app"`
}

// Secret is read from an environment variable or a file (a Docker secret).
type Secret struct {
	Env  string `yaml:"env"`
	File string `yaml:"file"`
}

// AppCredential is the GitHub App's installations on a client's organizations (the App's id and
// private key are the service's, from the environment). An installation token only reaches its
// own organization, so a project using it must keep its repos and Project in one organization.
type AppCredential struct {
	Installations map[string]int64 `yaml:"installations"` // organization -> installation id
}

// CredentialName returns the project's credential, DefaultCredential when it names none.
func (p *Project) CredentialName() string {
	if p.Credentials == "" {
		return DefaultCredential
	}
	return p.Credentials
}

// Project maps one Jiku project to its GitHub side.
type Project struct {
	// JikuProject is the id of the project in Jiku.
	JikuProject int64 `yaml:"jikuProject"`
	// Repos are the repos whose new issues become requirements ("owner/name").
	Repos []string `yaml:"repos"`
	// DefaultRepo is where the issues of requirements born in Jiku are created. It must be one of
	// Repos.
	DefaultRepo string `yaml:"defaultRepo"`
	// GithubProject is the node id of the Project v2 the Status (and the other fields) come from.
	// Optional: without it only open/closed is synced.
	GithubProject string `yaml:"githubProject"`
	// Credentials names an entry of File.Credentials: the client's GitHub credentials.
	Credentials string `yaml:"credentials"`
	// FieldMapping names a preset of File.FieldMappings. Ignored without GithubProject, except
	// for the issue type, which lives on the issue.
	FieldMapping string `yaml:"fieldMapping"`
	// InitialSync says what the service does with what already existed when the project was
	// mapped: "plan" (default: compute the plan and log it, write nothing), "apply" (run it once;
	// it is recorded in the state and never repeated) or "off".
	InitialSync string `yaml:"initialSync"`

	// Fields is the resolved preset (nil when none). Filled by Load.
	Fields *FieldMapping `yaml:"-"`
}

// Initial sync modes.
const (
	InitialSyncPlan  = "plan"
	InitialSyncApply = "apply"
	InitialSyncOff   = "off"
)

// InitialSyncMode returns the project's mode, "plan" when unset.
func (p *Project) InitialSyncMode() string {
	if p.InitialSync == "" {
		return InitialSyncPlan
	}
	return p.InitialSync
}

// FieldMapping says how the fields of a GitHub Project (and the issue type) translate to Jiku.
// Every part is optional: a missing part is simply not synced.
type FieldMapping struct {
	Status   *StatusMapping   `yaml:"status"`
	Priority *PriorityMapping `yaml:"priority"`
	Type     *TypeMapping     `yaml:"type"`
}

// StatusMapping maps the single-select Status field to Jiku's state.
type StatusMapping struct {
	// Field is the name of the single-select field.
	Field string `yaml:"field"`
	// ToJiku maps an option name to a Jiku state. Several options can map to the same state.
	ToJiku map[string]StatusTarget `yaml:"toJiku"`
	// ToGithub maps a Jiku state to one option name. A null value means "leave the field as it
	// is" (typical for `cancelado`, which the issue's closing already expresses).
	ToGithub map[string]*string `yaml:"toGithub"`
}

// StatusTarget is where an option lands in Jiku: a state, and optionally an internal comment
// to leave on the requirement (e.g. for an option Jiku has no state for). In YAML it is either a
// plain state ("en_cola") or {state: en_cola, comment: "..."}; the comment may use {login} and
// {option}.
type StatusTarget struct {
	State   string `yaml:"state"`
	Comment string `yaml:"comment"`
}

// UnmarshalYAML accepts the short form (a bare state).
func (t *StatusTarget) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		t.State = n.Value
		return nil
	}
	type plain StatusTarget
	return n.Decode((*plain)(t))
}

// PriorityMapping maps a single-select field to Jiku's priority.
type PriorityMapping struct {
	Field string `yaml:"field"`
	// Map is Jiku priority -> option name. A null value means "the field empty".
	Map map[string]*string `yaml:"map"`
	// ToJiku adds option -> priority entries for options with no Jiku counterpart in Map
	// (e.g. a "Lowest" that should read as sin_prioridad).
	ToJiku map[string]string `yaml:"toJiku"`
}

// TypeMapping maps the issue type (and, as a fallback, labels) to Jiku's requirement type.
type TypeMapping struct {
	// Sources is the order of precedence when reading GitHub: "issueType", "label".
	Sources []string `yaml:"sources"`
	// IssueType maps an issue type name to a Jiku type.
	IssueType map[string]string `yaml:"issueType"`
	// Label maps a label name to a Jiku type.
	Label map[string]string `yaml:"label"`
	// ToGithub maps a Jiku type to an issue type name; null leaves the issue without type.
	ToGithub map[string]*string `yaml:"toGithub"`
}

// Load reads and validates the configuration file.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is the operator's configuration
	if err != nil {
		return nil, fmt.Errorf("reading the mapping file: %w", err)
	}
	return Parse(b)
}

// Parse decodes and validates a configuration.
func Parse(b []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("mapping file: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) validate() error {
	var errs []error
	for name, fm := range f.FieldMappings {
		if fm == nil {
			errs = append(errs, fmt.Errorf("fieldMappings.%s is empty", name))
			continue
		}
		errs = append(errs, fm.validate("fieldMappings."+name)...)
	}
	for name, c := range f.Credentials {
		at := "credentials." + name
		switch {
		case c == nil || (c.Token == nil) == (c.App == nil):
			errs = append(errs, fmt.Errorf("%s needs exactly one of token or app", at))
		case c.Token != nil && (c.Token.Env == "") == (c.Token.File == ""):
			errs = append(errs, fmt.Errorf("%s.token needs exactly one of env or file", at))
		case c.App != nil && len(c.App.Installations) == 0:
			errs = append(errs, fmt.Errorf("%s.app.installations needs at least one organization", at))
		case c.App != nil:
			for org, id := range c.App.Installations {
				if id <= 0 {
					errs = append(errs, fmt.Errorf("%s.app.installations[%s]: invalid installation id", at, org))
				}
			}
		}
		if name == DefaultCredential {
			errs = append(errs, fmt.Errorf("%s: %q is reserved for GITHUB_TOKEN", at, DefaultCredential))
		}
	}
	seen := map[int64]bool{}
	repoOwner := map[string]int64{}
	for i, p := range f.Projects {
		at := fmt.Sprintf("projects[%d]", i)
		if p == nil {
			errs = append(errs, fmt.Errorf("%s is empty", at))
			continue
		}
		if p.JikuProject <= 0 {
			errs = append(errs, fmt.Errorf("%s.jikuProject is required", at))
		}
		if seen[p.JikuProject] {
			errs = append(errs, fmt.Errorf("%s: project %d is mapped twice", at, p.JikuProject))
		}
		seen[p.JikuProject] = true
		if len(p.Repos) == 0 {
			errs = append(errs, fmt.Errorf("%s.repos needs at least one repo", at))
		}
		for _, r := range p.Repos {
			if !validRepo(r) {
				errs = append(errs, fmt.Errorf("%s.repos: %q is not owner/name", at, r))
			}
			if other, ok := repoOwner[strings.ToLower(r)]; ok && other != p.JikuProject {
				errs = append(errs, fmt.Errorf("%s.repos: %s is already mapped to project %d", at, r, other))
			}
			repoOwner[strings.ToLower(r)] = p.JikuProject
		}
		switch {
		case len(f.Credentials) > 0 && p.Credentials == "":
			errs = append(errs, fmt.Errorf("%s.credentials is required: with a credentials section every project names its client's", at))
		case p.Credentials != "":
			c, ok := f.Credentials[p.Credentials]
			if !ok {
				errs = append(errs, fmt.Errorf("%s.credentials: %q does not exist", at, p.Credentials))
			} else if c != nil && c.App != nil {
				for _, r := range p.Repos {
					if !strings.EqualFold(Owner(r), Owner(p.DefaultRepo)) {
						errs = append(errs, fmt.Errorf("%s: with a GitHub App credential all repos must be in one organization (%s and %s)", at, Owner(r), Owner(p.DefaultRepo)))
						break
					}
				}
				if _, ok := c.App.Installation(Owner(p.DefaultRepo)); !ok {
					errs = append(errs, fmt.Errorf("%s: credentials %q has no installation for %s", at, p.Credentials, Owner(p.DefaultRepo)))
				}
			}
		}
		if p.DefaultRepo == "" {
			errs = append(errs, fmt.Errorf("%s.defaultRepo is required", at))
		} else if !p.HasRepo(p.DefaultRepo) {
			errs = append(errs, fmt.Errorf("%s.defaultRepo %q must be one of repos", at, p.DefaultRepo))
		}
		switch p.InitialSync {
		case "", InitialSyncPlan, InitialSyncApply, InitialSyncOff:
		default:
			errs = append(errs, fmt.Errorf("%s.initialSync: %q is not plan, apply or off", at, p.InitialSync))
		}
		if p.FieldMapping != "" {
			fm, ok := f.FieldMappings[p.FieldMapping]
			if !ok {
				errs = append(errs, fmt.Errorf("%s.fieldMapping: preset %q does not exist", at, p.FieldMapping))
			}
			p.Fields = fm
		}
	}
	return errors.Join(errs...)
}

func (fm *FieldMapping) validate(at string) []error {
	var errs []error
	if s := fm.Status; s != nil {
		if s.Field == "" {
			errs = append(errs, fmt.Errorf("%s.status.field is required", at))
		}
		for opt, t := range s.ToJiku {
			if !contains(JikuStates, t.State) {
				errs = append(errs, fmt.Errorf("%s.status.toJiku[%q]: %q is not a Jiku state", at, opt, t.State))
			}
		}
		for st := range s.ToGithub {
			if !contains(JikuStates, st) {
				errs = append(errs, fmt.Errorf("%s.status.toGithub: %q is not a Jiku state", at, st))
			}
		}
	}
	if p := fm.Priority; p != nil {
		if p.Field == "" {
			errs = append(errs, fmt.Errorf("%s.priority.field is required", at))
		}
		for pr := range p.Map {
			if !contains(JikuPriorities, pr) {
				errs = append(errs, fmt.Errorf("%s.priority.map: %q is not a Jiku priority", at, pr))
			}
		}
		for opt, pr := range p.ToJiku {
			if !contains(JikuPriorities, pr) {
				errs = append(errs, fmt.Errorf("%s.priority.toJiku[%q]: %q is not a Jiku priority", at, opt, pr))
			}
		}
	}
	if t := fm.Type; t != nil {
		for _, s := range t.Sources {
			if s != "issueType" && s != "label" {
				errs = append(errs, fmt.Errorf("%s.type.sources: %q is not issueType or label", at, s))
			}
		}
		for k, v := range t.IssueType {
			if !contains(JikuTypes, v) {
				errs = append(errs, fmt.Errorf("%s.type.issueType[%q]: %q is not a Jiku type", at, k, v))
			}
		}
		for k, v := range t.Label {
			if !contains(JikuTypes, v) {
				errs = append(errs, fmt.Errorf("%s.type.label[%q]: %q is not a Jiku type", at, k, v))
			}
		}
		for k := range t.ToGithub {
			if !contains(JikuTypes, k) {
				errs = append(errs, fmt.Errorf("%s.type.toGithub: %q is not a Jiku type", at, k))
			}
		}
	}
	return errs
}

// Project returns the mapping of a Jiku project, or nil.
func (f *File) Project(jikuProject int64) *Project {
	for _, p := range f.Projects {
		if p.JikuProject == jikuProject {
			return p
		}
	}
	return nil
}

// ProjectForRepo returns the mapping that owns a repo, or nil.
func (f *File) ProjectForRepo(repo string) *Project {
	for _, p := range f.Projects {
		if p.HasRepo(repo) {
			return p
		}
	}
	return nil
}

// HasRepo reports whether repo is one of the project's repos (case-insensitive, as GitHub).
func (p *Project) HasRepo(repo string) bool {
	for _, r := range p.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// StateFromOption maps a Status option to its Jiku target. ok is false when the option is not
// mapped (or there is no status mapping).
func (p *Project) StateFromOption(option string) (StatusTarget, bool) {
	if p.Fields == nil || p.Fields.Status == nil {
		return StatusTarget{}, false
	}
	t, ok := p.Fields.Status.ToJiku[option]
	return t, ok
}

// OptionForState maps a Jiku state to a Status option. set is false when the field must be left
// as it is (not mapped, or mapped to null).
func (p *Project) OptionForState(state string) (option string, set bool) {
	if p.Fields == nil || p.Fields.Status == nil {
		return "", false
	}
	v, ok := p.Fields.Status.ToGithub[state]
	if !ok || v == nil {
		return "", false
	}
	return *v, true
}

// PriorityFromOption maps a priority option ("" = empty field) to Jiku. ok is false when the
// option is unknown.
func (p *Project) PriorityFromOption(option string) (string, bool) {
	if p.Fields == nil || p.Fields.Priority == nil {
		return "", false
	}
	pm := p.Fields.Priority
	if pr, ok := pm.ToJiku[option]; ok {
		return pr, true
	}
	// Deterministic: when two priorities point to the same option, the first in Jiku's order wins.
	for _, pr := range JikuPriorities {
		v, ok := pm.Map[pr]
		if !ok {
			continue
		}
		if (v == nil && option == "") || (v != nil && *v == option) {
			return pr, true
		}
	}
	return "", false
}

// OptionForPriority maps a Jiku priority to an option ("" = clear the field). ok is false when
// the priority is not mapped.
func (p *Project) OptionForPriority(priority string) (string, bool) {
	if p.Fields == nil || p.Fields.Priority == nil {
		return "", false
	}
	v, ok := p.Fields.Priority.Map[priority]
	if !ok {
		return "", false
	}
	if v == nil {
		return "", true
	}
	return *v, true
}

// TypeFromGithub maps the issue type and labels to a Jiku type, following Sources. ok is false
// when nothing matches (or there is no type mapping).
func (p *Project) TypeFromGithub(issueType string, labels []string) (string, bool) {
	if p.Fields == nil || p.Fields.Type == nil {
		return "", false
	}
	tm := p.Fields.Type
	sources := tm.Sources
	if len(sources) == 0 {
		sources = []string{"issueType", "label"}
	}
	for _, s := range sources {
		switch s {
		case "issueType":
			if t, ok := tm.IssueType[issueType]; ok && issueType != "" {
				return t, true
			}
		case "label":
			ls := append([]string(nil), labels...)
			sort.Strings(ls)
			for _, l := range ls {
				if t, ok := tm.Label[l]; ok {
					return t, true
				}
			}
		}
	}
	return "", false
}

// IssueTypeForJiku maps a Jiku type to an issue type name. set is false when the issue type must
// be left as it is; name "" with set true means "remove the type".
func (p *Project) IssueTypeForJiku(jikuType string) (name string, set bool) {
	if p.Fields == nil || p.Fields.Type == nil || p.Fields.Type.ToGithub == nil {
		return "", false
	}
	v, ok := p.Fields.Type.ToGithub[jikuType]
	if !ok {
		return "", false
	}
	if v == nil {
		return "", true
	}
	return *v, true
}

// Owner returns the owner of an "owner/name" repo.
func Owner(repo string) string {
	o, _, _ := strings.Cut(repo, "/")
	return o
}

func validRepo(r string) bool {
	o, n, ok := strings.Cut(r, "/")
	return ok && o != "" && n != "" && !strings.Contains(n, "/")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Installation returns the installation id for an organization (case-insensitive, as GitHub).
func (a *AppCredential) Installation(org string) (int64, bool) {
	for o, id := range a.Installations {
		if strings.EqualFold(o, org) {
			return id, true
		}
	}
	return 0, false
}
