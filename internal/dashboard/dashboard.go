// Package dashboard renders a self-contained, offline HTML report.
package dashboard

import (
	_ "embed"
	"html/template"
	"io"

	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

//go:embed index.html
var page string

func Render(w io.Writer, r *report.Report) error {
	t, err := template.New("dashboard").Parse(page)
	if err != nil {
		return err
	}
	return t.Execute(w, r)
}
