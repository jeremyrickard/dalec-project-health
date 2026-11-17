// Package report defines the portable dashboard data contract.
package report

import "time"

const Unknown = "Unknown"

type Report struct {
	SchemaVersion string       `json:"schemaVersion"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Source        Source       `json:"source"`
	Partial       bool         `json:"partial"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
	Projects      []Project    `json:"projects"`
}

type Source struct {
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
	Scope    string `json:"scope"`
}

type Diagnostic struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
}

type Project struct {
	ID         string           `json:"id"`
	Repository string           `json:"repository"`
	Reason     string           `json:"reason"`
	Lifecycle  string           `json:"lifecycle"`
	GitHub     *RepositoryState `json:"github"`
	Components []Component      `json:"components"`
	LFX        Health           `json:"lfx"`
}

type RepositoryState struct {
	Archived    bool      `json:"archived"`
	RetrievedAt time.Time `json:"retrievedAt"`
	EvidenceURL string    `json:"evidenceUrl"`
}

type Component struct {
	Name     string    `json:"name"`
	Versions []Version `json:"versions"`
}

type Version struct {
	PackageVersion string      `json:"packageVersion"`
	Revision       string      `json:"revision"`
	Path           string      `json:"path"`
	Website        string      `json:"website"`
	Candidates     []Candidate `json:"candidates"`
	Ref            string      `json:"ref"`
	Targets        []string    `json:"targets"`
	Images         []Image     `json:"images"`
	Freshness      Freshness   `json:"freshness"`
}

type Candidate struct {
	Source     string `json:"source"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

type Image struct {
	Identity string   `json:"identity"`
	Targets  []string `json:"targets"`
}

type Freshness struct {
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	ResolvedTag  string     `json:"resolvedTag"`
	Commit       string     `json:"commit"`
	LatestStable string     `json:"latestStable"`
	LatestInLine string     `json:"latestInLine"`
	Update       string     `json:"update"`
	LineUpdate   bool       `json:"lineUpdate"`
	Date         *time.Time `json:"date"`
	AgeDays      *int       `json:"ageDays"`
	AgeKind      string     `json:"ageKind"`
	EvidenceURL  string     `json:"evidenceUrl"`
	RetrievedAt  *time.Time `json:"retrievedAt"`
}

type Health struct {
	Status          string     `json:"status"`
	Reason          string     `json:"reason"`
	Slug            string     `json:"slug"`
	Name            string     `json:"name"`
	Scope           string     `json:"scope"`
	IdentityGitHub  string     `json:"identityGitHub"`
	IdentityWebsite string     `json:"identityWebsite"`
	Lifecycle       string     `json:"lifecycle"`
	Score           *float64   `json:"score"`
	MaxScore        *float64   `json:"maxScore"`
	Label           *string    `json:"label"`
	Coverage        *int       `json:"coverage"`
	EvidenceURL     string     `json:"evidenceUrl"`
	RetrievedAt     *time.Time `json:"retrievedAt"`
}

func New(now time.Time) *Report {
	return &Report{
		SchemaVersion: "1", GeneratedAt: now.UTC(),
		Source:      Source{Revision: Unknown, Scope: "Definitions in the input checkout's specs/; not proof of build or publication"},
		Diagnostics: []Diagnostic{}, Projects: []Project{},
	}
}

func (r *Report) Failure(provider, subject string, err error) {
	r.Partial = true
	r.Diagnostics = append(r.Diagnostics, Diagnostic{provider, subject, err.Error()})
}
