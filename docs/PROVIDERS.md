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
- **Docs:** REST rate limits, "Best practices for using the REST API", Acceptable Use Policies, Terms of Service (Automated access / API terms). *Last verified: TBD at M3.*
- **Auth:** fine-grained PAT (preferred, `Contents: read`, `Metadata: read`, `Pull requests: read`), OAuth device flow, GitHub App (installation token, higher limits and no user dependency).
- **Limits (typical):** primary ~5 000 req/h per authenticated user (GraphQL has a separate point budget); unauthenticated far lower. **Secondary limits** (concurrency, requests/minute, content-creation) apply: keep to ≤ 1–2 concurrent requests, honour `Retry-After`.
- **Tactics:** `ETag`/`If-None-Match` (304s are free against primary quota), `GET /user/repos` with `per_page=100`, GraphQL to batch PR-merge checks, read `X-RateLimit-Remaining/Reset`.
- **Git over HTTPS** also counts toward abuse detection; use `ls-remote` to skip no-op fetches.

## GitLab (gitlab.com, self-managed)
- **Docs:** GitLab.com rate limits, API docs, Terms. *Last verified: TBD at M4.*
- **Auth:** PAT (`read_api`, `read_repository`), OAuth device grant, project/group access tokens for narrower scope.
- **Limits:** SaaS has per-user/IP API limits with `RateLimit-*` and `Retry-After`; self-managed limits are admin-configured: read headers, don't hard-code.
- **Tactics:** keyset pagination, `membership=true`, `simple=true`, `statistics=false`.

## Bitbucket Cloud
- **Docs:** Bitbucket Cloud REST API rate limiting, Atlassian ToS/AUP. Atlassian has been deprecating **app passwords** in favour of **API tokens**; target the current mechanism. *Last verified: TBD at M4.*
- **Auth:** API token / repository or workspace access token; OAuth 2.0 consumer.
- **Limits:** per-hour request limits with 429 + `Retry-After`; treat as low and cache aggressively.
- Bitbucket Data Center: separate REST API; later milestone.

## Azure DevOps (Services / Server)
- **Docs:** Rate and usage limits (resource-utilisation based, `Retry-After`, `X-RateLimit-*`/`X-RateLimit-Delay`), Microsoft ToS. *Last verified: TBD at M4.*
- **Auth:** Entra ID OAuth (preferred; Microsoft is steering away from PATs), PAT (`Code: Read`).
- **Tactics:** honour delay headers *proactively* (ADO throttles by cost, not count); org → project → repo discovery with continuation tokens.

## Gitea / Forgejo
- Instance-defined limits (often none): still apply global politeness rules. Token auth with `read:repository`, `read:user`, `read:organization`.

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
