package dashboard

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jeremyrickard/dalec-project-health/internal/report"
)

func TestSafeJSONAndSelfContainedHTML(t *testing.T) {
	attack := `</script><img src=x onerror=alert(1)>`
	r := report.New(time.Unix(0, 0))
	r.Projects = []report.Project{{
		ID: attack, Repository: attack, Components: []report.Component{{Name: attack}},
		Activity: &report.Activity{Band: attack, Reason: attack, Methodology: attack, Issues: []string{attack}},
	}}
	var out bytes.Buffer
	if err := Render(&out, r); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, attack) || strings.Contains(html, "innerHTML") {
		t.Fatal("unsafe HTML / DOM interpolation")
	}
	m := regexp.MustCompile(`(?s)<script id="report" type="application/json">(.*?)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatal("embedded report missing")
	}
	var decoded report.Report
	if err := json.Unmarshal([]byte(m[1]), &decoded); err != nil {
		t.Fatal("embedded JSON invalid", err)
	}
	if decoded.Projects[0].ID != attack {
		t.Fatal("data changed by embedding")
	}
	if decoded.Projects[0].Activity.Methodology != attack {
		t.Fatal("activity text changed by embedding")
	}
	for _, external := range []string{`<script src=`, `<link rel="stylesheet"`, "fetch(", "XMLHttpRequest", "@import"} {
		if strings.Contains(html, external) {
			t.Errorf("runtime external dependency: %s", external)
		}

	}
	if !strings.Contains(html, "--cp-accent:") || !strings.Contains(html, `"Segoe UI"`) {
		t.Fatal("required theme/font missing")
	}
}

func TestOlderReportWithoutActivityStillRenders(t *testing.T) {
	r := report.New(time.Unix(0, 0))
	r.SchemaVersion = "1"
	r.Projects = []report.Project{{ID: "older", Components: []report.Component{}}}
	var out bytes.Buffer
	if err := Render(&out, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"activity":null`) || !strings.Contains(out.String(), `if(p.activity)`) {
		t.Fatal("optional activity contract is not backward compatible")
	}
}
