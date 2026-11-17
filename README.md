# DALEC project health

A standalone Go tool for exploring upstream projects, defined versions, container
image identities, and evidence-backed health signals in a DALEC build-definitions
checkout. It generates an offline dashboard and a machine-readable JSON snapshot.
It does not invoke azcu, build images, or verify publication to MCR.

## Build and install

Go 1.26.8 or newer is required (Go's automatic toolchain selection also works).
The tool's source repository is public; its **azcu library dependency is private**.
Building requires a GitHub account authorized to read `Azure/dalec-build-defs`.
Authenticate with GitHub CLI first, including any organization SSO authorization.

Clone this repository, then run these process-scoped commands:

```sh
git clone https://github.com/jeremyrickard/dalec-project-health.git
cd dalec-project-health

GOPRIVATE=github.com/Azure/dalec-build-defs \
GIT_CONFIG_COUNT=1 \
GIT_CONFIG_KEY_0=credential.helper \
GIT_CONFIG_VALUE_0='!gh auth git-credential' \
go build ./cmd/dalec-project-health
```

Use the same environment prefix with `go install ./cmd/dalec-project-health`
to install to your Go bin directory. This does not change global Git or Go
configuration, write tokens to disk, or disable public-module checksum checks.
If you already use `GOPRIVATE` or process-scoped Git configuration, merge these
values with your existing configuration rather than overwriting them.

The root `github.com/Azure/dalec-build-defs` module is pinned to immutable revision
`a10619b69647844588cc66571fe90761761deb8b`
(`v0.0.0-20261117150922-a10619b69647`). **azcu is not a separate module.**
The consumer `go.mod` includes the necessary `github.com/vito/progrock` replacement
with `github.com/cpuguy83/progrock` at
`v0.0.0-20241017170618-ebe885ee99d3`, since dependency replacements are not inherited.
There are no local-path replacements, vendored private sources, or azcu subprocesses.
Dependency licenses remain with their respective modules; no dependency implementation
is copied into this repository.

## Generate a dashboard

Obtain an authorized checkout of the build definitions separately. The input is
always explicit; no inventory is read from the dependency's module cache or vendor
directory.

```sh
./dalec-project-health generate \
  --build-defs-dir /path/to/dalec-build-defs \
  --output-dir /path/to/private-reports/latest
```

Open `index.html` directly in a browser. It embeds all CSS, JavaScript, and data;
no web server, CDN, runtime API requests, or frontend toolchain is required.
`project-health.json` contains the same report, with `schemaVersion: "1"`.
The HTML contains the full inventory too: **treat both outputs as private whenever
the input definitions are private**. Do not publish real reports or screenshots
without authorization.

For GitHub enrichment, set `GITHUB_TOKEN` (preferred) or `GH_TOKEN` in the
environment. Only read-only upstream repository, tag, release, and commit APIs
are used. Without a token GitHub's unauthenticated limits apply. LFX uses only its
supported public unauthenticated endpoints. Dependency build authentication and
runtime enrichment authentication are independent.

`--timeout 15m` is the default total generation budget. Each upstream gets a
two-minute budget; individual HTTP requests have a 25-second timeout. Requests
are deduplicated and paced (GitHub at approximately 9 requests/second; LFX below
its default 200 requests/60 seconds). HTTP 429, rate-limit 403, transport failures,
and 5xx responses get at most two retries. `Retry-After` and rate-limit reset
headers are honored; waits longer than five seconds fail explicitly instead of
blocking indefinitely. Cancellation stops pending work.

### Exit and failure semantics

- Invalid arguments, unreadable/empty input, malformed specs, and output errors
  fail instead of writing a success-shaped empty report.
- External operational failures preserve the complete local inventory, save an
  explicitly partial report, emit warnings, and **exit nonzero after saving**.
- Unsupported upstream hosts, ambiguous identities, and expected missing LFX
  coverage remain visible as Unknown; those are not operational failures.
- Output files are staged before replacement, with each file atomically renamed.
  An output error is always fatal; consumers should only use a generation after
  the command reports it was saved (or explicitly reports a saved partial result).

## What the report means

**Inventory.** `internal/inventory` is a thin adapter around exported azcu
`spec.WalkSpecs`, `spec.FromFile`, `spec.LoadExtensions`, `spec.ImageName`, and
`spec.DefaultBuildTargets`. `FromFile` resolves default argument substitutions.
`ImageName` uses `BuildExtensions.ForTarget` for target-specific overrides.
Only production YAML under the input's `specs/` is inventoried: the library
excludes test/template directories and matrix definitions. Production spec
symlinks are rejected rather than reading arbitrary external files.
Ancillary YAML (for example vendored registry presubmit configuration) without
a DALEC package identity signature is excluded with a visible diagnostic. A
signature is at least two of `name`, `version`, `revision`, `license`, or the
presence of `revision` or `x-build-extensions`. Identified specs still must parse
and contain a name, version and revision; an inventory with no production specs fails.

Canonical upstream repositories group components, which retain every spec
version/variant, package revision, source ref, spec path, and target. Git URLs
are normalized across HTTPS, SSH, and SCP forms; credentials, query strings, and
fragments are discarded. A sole Git repository identifies upstream. For multiple
repositories, an exact repository website match or a uniquely identifying raw
`${COMMIT}` source reference must select one repository without conflicting
evidence. Source ordering, map ordering, filenames, and similarity are never
identity evidence. Ambiguous and no-Git specs remain visible with reasons and
candidates. Multiple selected-repository refs leave version enrichment Unknown.

Container identities use effective per-target repository/image overrides and
default build targets, deduplicating identical images while retaining targets.
Package-only definitions explicitly show **No container image**.
This is what the input checkout **defines**, not proof of what was built/published.
The source revision, modified-spec status, and generation time are included.

**GitHub freshness.** Exported azcu GitHub helpers handle tag pagination and
repository parsing; `go-github` supplies repository, paginated releases, and
commit operations. Source refs/tags or exact commit-to-tag associations identify
the upstream version. Package versions are never assumed to equal upstream tags.
Ambiguous commit-to-tag associations remain Unknown. Stable comparisons use
numeric semantic versions, excluding drafts/prereleases, and keep exact
component-prefixed monorepo streams separate. Both the latest stable in the
selected stream and the latest stable in the same major.minor line are shown,
with patch/minor/major update significance. Non-semver streams remain Unknown.
Age uses a published release timestamp, falling back explicitly to **commit age**;
absent dates remain Unknown. No support/EOL state is inferred from age or lag.
Non-GitHub source inventory works, but GitHub enrichment is unavailable.
GitHub archival is retained as a separate repository signal, not equated with EOL.

**LFX Insights.** Only
[`/api/project` and `/api/project/{slug}/insights`](https://raw.githubusercontent.com/linuxfoundation/insights/main/api/docs/site/public/web-api.json)
are used. Repository, organization, and component names generate candidate slugs
automatically. These are lookup hints, **not proof**: matches require an exact
repository link or corroborating exact project website. Org-only links,
unmatched aliases (including non-obvious slugs), conflicting repository links,
and multiple matches remain Unknown. There is no curated mapping file and no
documented repository-search API; this intentionally limits enrichment coverage.

The native LFX activity lifecycle (active, stable, declining, inert, abandoned,
archived) is displayed **separately from support lifecycle**. Maintenance and EOL
remain Unknown without authoritative support evidence; this version does not
scrape or invent support policies. Native `healthScoreV2`, `healthMaxScore`,
`healthLabel`, and category coverage are preserved. Scores can be out of 60, 65,
75, or 100; null is unscored, never zero. Evidence links and retrieval timestamps
are included, and multi-repository project assessments are labeled as such.
The live API can omit assessment type/repository fields present in its published
schema. A verified slug still supplies health metrics, but absent assessment
scope is explicitly **Unknown (may span repositories)**, never silently treated
as a score for the selected repository.

**Dashboard.** Search across upstream/component/path/image; filter support
lifecycle, LFX activity, update significance, or missing enrichment; sort by
project, known age, or update significance. Expand groups for per-version image
identities and source evidence. Light/dark themes use the Clawpilot palette;
`?clawpilotTheme=light` or `?clawpilotTheme=dark` chooses the initial theme.
All untrusted report strings are safely JSON-encoded by `html/template` and
rendered with DOM text nodes, not HTML interpolation.

## Development

After authorized module download, run:

```sh
GOPRIVATE=github.com/Azure/dalec-build-defs go test ./...
GOPRIVATE=github.com/Azure/dalec-build-defs go vet ./...
GOPRIVATE=github.com/Azure/dalec-build-defs go build ./cmd/dalec-project-health
```

Tests use original synthetic specs, injected providers/clocks, and HTTP test
servers. No real build-definition data is committed. To generate an enriched
synthetic dashboard without network access:

```sh
GOPRIVATE=github.com/Azure/dalec-build-defs \
DALEC_HEALTH_SMOKE_DIR=/tmp/dalec-health-fixture \
go test ./internal/app -run '^TestGenerateFixture$' -count=1
```

For an opt-in, read-only inventory smoke test against your own checkout:

```sh
GOPRIVATE=github.com/Azure/dalec-build-defs \
DALEC_HEALTH_CHECKOUT=/path/to/dalec-build-defs \
go test ./internal/inventory -run '^TestReadOnlyCheckoutSmoke$' -count=1 -v
```

Set `DALEC_HEALTH_EXPECTED_SPEC_COUNT` to an independently established count to
assert inventory completeness. This test makes no enrichment API requests and
does not save or publish the input data.

The tool has no scheduler, service, database, hosting configuration, CVE scanner,
team/product mapping, or Service Tree integration.
