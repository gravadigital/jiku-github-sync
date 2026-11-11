package mapping

import (
	"strings"
	"testing"
)

func loadExample(t *testing.T) *File {
	t.Helper()
	f, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	return f
}

func TestExampleConfigIsValid(t *testing.T) {
	f := loadExample(t)
	p := f.Project(123)
	if p == nil || p.Fields == nil {
		t.Fatal("project 123 with its preset expected")
	}
	if f.ProjectForRepo("ACME-ORG/example-web") != p {
		t.Fatal("repo lookup must be case-insensitive")
	}
}

func TestStatus(t *testing.T) {
	p := loadExample(t).Project(123)
	if tg, ok := p.StateFromOption("Ideas"); !ok || tg.State != "analisis" {
		t.Fatalf("Ideas -> %v %v", tg, ok)
	}
	if tg, ok := p.StateFromOption("Blocked"); !ok || tg.State != "en_cola" || !strings.Contains(tg.Comment, "{login}") {
		t.Fatalf("Blocked long form -> %+v", tg)
	}
	if _, ok := p.StateFromOption("QA"); ok {
		t.Fatal("an unmapped option must report !ok")
	}
	if opt, set := p.OptionForState("revision"); !set || opt != "Review" {
		t.Fatalf("revision -> %q %v", opt, set)
	}
	if _, set := p.OptionForState("cancelado"); set {
		t.Fatal("null in toGithub means leave the field as it is")
	}
}

func TestPriority(t *testing.T) {
	p := loadExample(t).Project(123)
	cases := map[string]string{"Critical": "urgente", "": "sin_prioridad", "Lowest": "sin_prioridad", "Low": "baja"}
	for opt, want := range cases {
		if got, ok := p.PriorityFromOption(opt); !ok || got != want {
			t.Errorf("%q -> %q %v, want %q", opt, got, ok, want)
		}
	}
	if opt, ok := p.OptionForPriority("sin_prioridad"); !ok || opt != "" {
		t.Errorf("sin_prioridad -> %q %v, want clear", opt, ok)
	}
}

func TestType(t *testing.T) {
	p := loadExample(t).Project(123)
	if got, ok := p.TypeFromGithub("Bug", []string{"enhancement"}); !ok || got != "incidencia" {
		t.Errorf("issue type wins: %q", got)
	}
	if got, ok := p.TypeFromGithub("", []string{"enhancement"}); !ok || got != "mejora" {
		t.Errorf("label fallback: %q", got)
	}
	if _, ok := p.TypeFromGithub("", nil); ok {
		t.Error("nothing matches -> !ok")
	}
	if name, set := p.IssueTypeForJiku("otro"); !set || name != "" {
		t.Errorf("otro -> remove type, got %q %v", name, set)
	}
}

func TestValidationErrors(t *testing.T) {
	bad := `
fieldMappings:
  x:
    status:
      field: Status
      toJiku: {"A": nope}
      toGithub: {wrong: "A"}
projects:
  - jikuProject: 1
    repos: [a/b]
    defaultRepo: c/d
    fieldMapping: missing
  - jikuProject: 2
    repos: [a/b, bad]
    defaultRepo: a/b
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{`"nope" is not a Jiku state`, `"wrong" is not a Jiku state`, "must be one of repos",
		`preset "missing" does not exist`, "already mapped to project 1", `"bad" is not owner/name`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q in:\n%v", want, err)
		}
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	_, err := Parse([]byte("projects:\n  - jikuProject: 1\n    repo: a/b\n"))
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("a typo in a key must fail, got %v", err)
	}
}
