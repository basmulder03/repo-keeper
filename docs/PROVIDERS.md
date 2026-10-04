# Providers, authentication, rate limits & ToS compliance

> **Verify before implementing.** Limits and policies change. Each section carries a *last verified* date; the person implementing a provider must re-check the linked official docs and update the date. Figures below are starting points, not guarantees.

## Global compliance rules (all providers)

1. Use only documented public REST/GraphQL APIs and the standard git protocols (HTTPS/SSH). No scraping HTML, no private endpoints.
2. Send `User-Agent: repo-keeper/<version> (+https://github.com/basmulder03/repo-keeper)`.
3. Authenticate as the **user themselves** with their own credentials/consent. Never pool several accounts to multiply quota; never rotate tokens to dodge limits (CMP-5).
4. Obey `Retry-After`, rate-limit headers and any "secondary/abuse" signals; back off on 403/429/5xx.
5. Prefer conditional requests, `ls-remote`, and long intervals over polling. Default 30 min, minimum 5 min, discovery every 6 h.
6. Serialise requests per host (low concurrency); avoid bursts at the top of the hour (jitter).
7. Request least-privilege, read-only scopes. Document exactly why each scope is needed.
8. Don't mirror/redistribute content; clones stay on the user's machine for the user's own use. Respect repo-level restrictions (e.g. LFS bandwidth quotas).
9. If a provider requests automated-access registration (OAuth app / GitHub App), register once for the project and publish the details; allow users to bring their own client ID.

## Capability matrix (target)

| | GitHub | GitLab | Bitbucket Cloud | Azure DevOps | Gitea/Forgejo | Generic git |
|---|---|---|---|---|---|---|
| Discovery | REST | REST | REST | REST | REST | static list |
| PAT/token | ✔ | ✔ | ✔ (API token / repo & workspace access tokens) | ✔ | ✔ | n/a |
| OAuth device flow | ✔ | ✔ (device auth grant) | ✘ (auth-code; use token) | Entra ID | partial | n/a |
| App/installation auth | GitHub App | – | – | – | – | n/a |
| SSH | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ |
| Merged-PR lookup | ✔ | ✔ | ✔ | ✔ | ✔ | ✘ (use `git cherry`) |
| Self-hosted | GHES | self-managed | Data Center (separate API) | Server | ✔ | ✔ |

## GitHub (github.com, GHES)

**Implemented (M3a).** Endpoints used, all documented REST: `GET /user` (auth, scopes, expiry), `GET /user/repos` (discovery, `per_page=100`, `Link` pagination, ETag), `GET /repos/{owner}/{repo}/pulls?state=closed` (only for upstream-deleted branches git cannot prove merged; at most 3 pages). Device flow: `POST /login/device/code`, `POST /login/oauth/access_token`. Requests carry `Authorization: Bearer`, `X-GitHub-Api-Version`, the project User-Agent; the token is never sent to a host other than the configured API host (pagination links to other hosts are refused).

**Recommended credential:** a fine-grained personal access token with *read-only* `Contents`, `Metadata` and `Pull requests` on the repositories you want. Classic tokens need the broad `repo` scope for private repositories, so `accounts add` and `accounts check` warn about it. Device flow works best with a GitHub App (client id via `--client-id`, no scope, read-only permissions); **disable user-token expiration for that app**, because refreshing needs a client secret and repo-keeper deliberately ships none.

**GitHub Enterprise Server:** `base_url = "https://ghe.example.com/api/v3"`, and `--web-url https://ghe.example.com` for device flow.
- **Docs:** REST rate limits, "Best practices for using the REST API", Acceptable Use Policies, Terms of Service (Automated access / API terms). *Last verified: TBD at M3.*
- **Auth:** fine-grained PAT (preferred, `Contents: read`, `Metadata: read`, `Pull requests: read`), OAuth device flow, GitHub App (installation token, higher limits and no user dependency).
- **Limits (typical):** primary ~5 000 req/h per authenticated user (GraphQL has a separate point budget); unauthenticated far lower. **Secondary limits** (concurrency, requests/minute, content-creation) apply: keep to ≤ 1–2 concurrent requests, honour `Retry-After`.
- **Tactics:** `ETag`/`If-None-Match` (304s are free against primary quota), `GET /user/repos` with `per_page=100`, GraphQL to batch PR-merge checks, read `X-RateLimit-Remaining/Reset`.
- **Git over HTTPS** also counts toward abuse detection; use `ls-remote` to skip no-op fetches.

## GitLab (gitlab.com, self-managed)

**Implemented (M4).** REST API v4 only. Endpoints: `GET /user`, `GET /personal_access_tokens/self` (scopes and expiry; absent for OAuth/project tokens, which is fine), `GET /projects?membership=true&pagination=keyset&per_page=100` (discovery, `Link` rel=next), `GET /projects/{url-encoded full path}/merge_requests?state=merged` (only for upstream-deleted branches git cannot prove merged; at most 3 pages). Requests use `Authorization: Bearer` (accepted for personal, project and group access tokens), the project User-Agent, and never send the token to another host.

**Nested groups:** `acme/platform/infra/terraform` is cloned to `<root>/gitlab/acme/platform/infra/terraform`. Include/exclude globs match the full path: `*` stays within one level, `**` crosses levels (`acme/**`). Empty projects are skipped until they have a commit. Archived projects are skipped by default (`skip_archived`), forks are kept unless `skip_forks = true`.

**Credentials:** a personal access token with `read_api` + `read_repository`, or better a *group/project access token* with the Reporter role. `accounts add` and `accounts check` warn about `api`/`write_repository` scopes and impending expiry. Git over HTTPS uses the username `oauth2` with the token. **Device flow is not implemented for GitLab yet**: its tokens expire after two hours and need a stored refresh token, which warrants its own design.

**Self-managed:** `base_url = "https://gitlab.example.com"` (the `/api/v4` suffix is added for you). Private CA support arrives with the hardening milestone.
- **Docs:** GitLab.com rate limits, API docs, Terms. *Last verified: TBD at M4.*
- **Auth:** PAT (`read_api`, `read_repository`), OAuth device grant, project/group access tokens for narrower scope.
- **Limits:** SaaS has per-user/IP API limits with `RateLimit-*` and `Retry-After`; self-managed limits are admin-configured: read headers, don't hard-code.
- **Tactics:** keyset pagination, `membership=true`, `simple=true`, `statistics=false`.

## Bitbucket Cloud

**Implemented (M6, `bitbucket` kind; fixture-tested only).** Documented REST 2.0 only, checked against Atlassian's published OpenAPI document (`https://api.bitbucket.org/swagger.json`) on 2026-10-04: `GET /user` (auth; scopes from the `X-OAuth-Scopes` header when sent), `GET /user/workspaces` then `GET /repositories/{workspace}?role=member` (discovery: **Bitbucket has no cross-workspace repository listing**, so every workspace the user belongs to is walked and `next` links are followed, never constructed), `GET /repositories/{workspace}/{repo}/pullrequests?state=MERGED` (merged-PR lookup: source and destination repository must be the same, so fork PRs are ignored) and `GET /repositories/{workspace}/{repo}/commit/{hash}`. Pull requests report short commit hashes and cleanup compares exact ids, so the short hash is expanded through the commit endpoint; a hash that cannot be resolved to a full id is left out (fail closed).
- **Auth:** an Atlassian **API token** sent as `Authorization: Bearer` (Bearer support for API tokens was announced 2026-08-18). **App passwords no longer work** (final removal 2026-07-28, per the Bitbucket changelog). Create the token with only `read:repository:bitbucket`, `read:pullrequest:bitbucket`, `read:user:bitbucket` and `read:workspace:bitbucket`. No OAuth device flow (Bitbucket has none); paste a token.
- **Mapping:** workspaces are the single namespace level (`workspace/repo-slug`); projects are not a path level. Mercurial repositories (`scm` other than `git`) are skipped; a repository without a main branch is `Disabled` (empty); Bitbucket has no archive state. The `https` clone link Bitbucket returns embeds `user@`; it is stripped so the credential only ever comes from askpass.
- **Limits:** Bitbucket enforces per-hour limits with 429 and `Retry-After`, honoured by the shared `httpx` layer. The exact numbers were not found in the machine-readable spec and are **unverified**; re-read the rate-limit page before 1.0.
- **Not yet verified against a live account:** the git username for HTTPS (`x-bitbucket-api-token-auth`, from memory of Atlassian's docs), the exact scope names an API token needs for `/user/workspaces`, repository/workspace *access tokens* (they are not tied to a user, so `/user` and `/user/workspaces` may refuse them and discovery would need a different path), and the `X-OAuth-Scopes` header on API tokens. Tracked in OPEN-QUESTIONS; ToS pages not re-read for this change.

## Azure DevOps (Services / Server)

**Implemented (M6, `azuredevops` kind; fixture-tested only).** REST 7.1, checked against Microsoft's published OpenAPI documents (`MicrosoftDocs/vsts-rest-api-specs`) on 2026-10-04: `GET {org}/_apis/projects` (continuation token in the `x-ms-continuationtoken` header), `GET {org}/{project}/_apis/git/repositories` per project (the documented list requires a project, so every project is walked) and `GET {org}/{project}/_apis/git/repositories/{repo}/pullrequests?searchCriteria.status=completed` (`$top`/`$skip` paging). A pull request counts as merged when it is `completed`, comes from a branch of the same repository (`forkSource` empty) and reports a full `lastMergeSourceCommit.commitId`. `base_url` is **required**: the organization (`https://dev.azure.com/acme`) or, for Server, the collection (`https://tfs.example.com/tfs/DefaultCollection`). Repositories are named `project/repository`; names may contain spaces (the clone path allows interior spaces, never leading or trailing ones).
- **Auth:** a **PAT** sent as Basic with an empty user. Use an **organization-scoped** PAT: Microsoft retires *global* PATs on **2026-12-01**. Needed scopes: **Code (Read)** (`vso.code`) and **Project and Team (Read)** (`vso.project`, for the project list). Microsoft Entra ID tokens and OAuth are not implemented yet. A bad or expired PAT is often answered with HTTP 203 and an HTML sign-in page rather than 401; that is treated as an authentication failure.
- **What `accounts check` can tell you:** Azure DevOps does not report a token's owner, scopes or expiry through the documented API, so "authenticated" means the token can list the organization's projects, the login shown is the organization, and a reminder about minimal scopes and short expiry is printed.
- **Mapping:** repositories of public projects are not private; `isDisabled` and repositories without a default branch are `Disabled`; there is no archive state; the `https` clone URL's embedded `org@` is stripped (credentials come from askpass). `git` over HTTPS uses user `pat`.
- **Limits:** Azure DevOps throttles by resource cost and answers with `Retry-After` (honoured by the shared `httpx` layer) and `X-RateLimit-*` headers. Proactive handling of `X-RateLimit-Delay` is **not** implemented yet; the exact limits were not re-read for this change.
- **Not yet verified against a live organization:** the `203` sign-in behaviour for every kind of bad token, the pull-request ordering (newest first is assumed; only the 300 most recent completed PRs are examined, anything beyond is simply not proven merged), the exact PAT scope names needed for the project list, and the Server flavour. Tracked in OPEN-QUESTIONS; ToS pages not re-read for this change.

## Gitea / Forgejo

**Implemented (M6, `gitea` and `forgejo` kinds; Codeberg is Forgejo).** Documented REST v1 only: `GET /user` (auth), `GET /user/repos` (discovery, `limit=50` = the default server cap, `Link` pagination), `GET /repos/{owner}/{repo}/pulls?state=closed` (merged-PR lookup: `merged`, `head.ref/sha`, head repo id must equal base repo id so forks are ignored). Token goes in `Authorization: token <t>`; git uses it as the password with user `oauth2`. `base_url` is **required** (no single public host), e.g. `https://codeberg.org`; the API path `/api/v1` is appended.
- **Credential:** an access token with only `read:repository` (+ `read:user`). Tokens cannot report their own scopes or expiry, so `accounts check` shows the login only and a reminder to keep scopes minimal. No OAuth device flow (Gitea has none); paste a token.
- **Limits:** instance-defined (often none); the global politeness rules (per-host pacing, `Retry-After`, backoff, breaker) still apply.
- **Namespaces:** always `owner/name` (organisations are owners); the merged-PR lookup refuses anything else rather than guessing.
- **Last verified:** 2026-10-03 against the API shape in fixtures only. **Not yet exercised against a live Gitea/Forgejo/Codeberg server and ToS pages not re-read for this change**; do both before promoting out of beta (tracked in OPEN-QUESTIONS).
## Generic git
- User supplies URLs; no discovery, no PR data. Auth via SSH agent or credential helper-equivalent from our keychain.

## Credential handling per provider
- Stored in OS keychain keyed by `repo-keeper/<account-id>`.
- Passed to git through a short-lived askpass helper (the repo-keeper binary re-invoked as `repo-keeper askpass`) that fetches the secret from an in-memory/ keychain source; never in argv, URL, or inherited env.
- OAuth refresh tokens rotated and re-stored atomically; on `invalid_grant` mark account "needs attention".

## Compliance checklist (copy per provider PR)
- [ ] ToS/AUP/API terms re-read; link + date updated here
- [ ] Rate-limit headers parsed; fixtures cover 429/403-secondary/Retry-After
- [ ] Minimal scopes documented in user docs
- [ ] Conditional requests implemented
- [ ] UA header verified in contract test
- [ ] No endpoints outside the public docs
