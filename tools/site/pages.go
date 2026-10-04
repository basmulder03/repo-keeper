// SPDX-License-Identifier: Apache-2.0

package main

// page is one output page: either a Markdown file from the repository or a generated body.
type page struct {
	Slug    string // output file name without extension
	Title   string
	Section string
	Source  string // repo-relative Markdown path; empty for generated pages
	Summary string // one line shown on the home page cards and in the page header
	gen     func(*build) (string, error)
}

// sections fixes the navigation order.
var sections = []string{"Start", "Reference", "Design", "Project"}

func pageList() []page {
	return []page{
		{Slug: "index", Title: "Overview", Section: "Start", Source: "README.md", Summary: "What repo-keeper is and how to try it."},
		{Slug: "install", Title: "Install", Section: "Start", Source: "docs/INSTALL.md", Summary: "Nix, Home Manager, packages and the tarball."},
		{Slug: "beta", Title: "Beta guide", Section: "Start", Source: "docs/BETA.md", Summary: "Test it safely: look first, sync one thing, clean last."},
		{Slug: "cli", Title: "Command line", Section: "Reference", Summary: "Every command and flag, generated from the binary.", gen: genCLI},
		{Slug: "config", Title: "Configuration", Section: "Reference", Summary: "The starter configuration, generated from the binary.", gen: genConfig},
		{Slug: "providers", Title: "Providers", Section: "Reference", Source: "docs/PROVIDERS.md", Summary: "GitHub, GitLab, Gitea/Forgejo, Bitbucket, Azure DevOps and plain git."},
		{Slug: "changelog", Title: "Release notes", Section: "Reference", Source: "CHANGELOG.md", Summary: "What changed in each release.", gen: genReleases},
		{Slug: "releasing", Title: "Releasing", Section: "Reference", Source: "docs/RELEASING.md", Summary: "How a release is cut and verified."},
		{Slug: "requirements", Title: "Requirements", Section: "Design", Source: "docs/REQUIREMENTS.md", Summary: "Scope and acceptance criteria."},
		{Slug: "architecture", Title: "Architecture", Section: "Design", Source: "docs/ARCHITECTURE.md", Summary: "How the pieces fit together."},
		{Slug: "threat-model", Title: "Threat model", Section: "Design", Source: "docs/THREAT-MODEL.md", Summary: "What can go wrong and what stops it."},
		{Slug: "security-review", Title: "Security review", Section: "Design", Source: "docs/SECURITY-REVIEW.md", Summary: "Findings and their status."},
		{Slug: "security", Title: "Security policy", Section: "Design", Source: "SECURITY.md", Summary: "Report a vulnerability."},
		{Slug: "distribution", Title: "Distribution", Section: "Design", Source: "docs/DISTRIBUTION.md", Summary: "Packaging, signing and supply chain."},
		{Slug: "adr", Title: "Decisions (ADRs)", Section: "Design", Source: "docs/adr/0001-decisions.md", Summary: "Why things are the way they are."},
		{Slug: "testing", Title: "Testing", Section: "Design", Source: "docs/TESTING.md", Summary: "How it is tested."},
		{Slug: "performance", Title: "Performance", Section: "Design", Source: "docs/PERFORMANCE.md", Summary: "Budgets and measurements."},
		{Slug: "contributing", Title: "Contributing", Section: "Project", Source: "CONTRIBUTING.md", Summary: "How to propose and land a change."},
		{Slug: "conduct", Title: "Code of conduct", Section: "Project", Source: "CODE_OF_CONDUCT.md", Summary: "How we treat each other."},
		{Slug: "agents", Title: "Agent instructions", Section: "Project", Source: "AGENTS.md", Summary: "The hard rules for AI coding agents working on this repository."},
		{Slug: "roadmap", Title: "Roadmap", Section: "Project", Source: "docs/ROADMAP.md", Summary: "Milestones and what is next."},
		{Slug: "plan", Title: "Implementation plan", Section: "Project", Source: "docs/IMPLEMENTATION-PLAN.md", Summary: "How the work was phased."},
		{Slug: "open-questions", Title: "Open questions", Section: "Project", Source: "docs/OPEN-QUESTIONS.md", Summary: "Decisions and verifications still pending."},
		{Slug: "docs-index", Title: "Reading order", Section: "Project", Source: "docs/README.md", Summary: "Where to start in the design documents."},
		{Slug: "stats", Title: "Statistics", Section: "Project", Summary: "Size, tests, coverage and releases, generated on every build.", gen: genStats},
	}
}
