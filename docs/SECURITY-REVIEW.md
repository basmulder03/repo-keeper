# Security self-review (milestone M7)

A structured pass over the whole code base against [THREAT-MODEL](THREAT-MODEL.md), done by the maintainers (an AI-assisted self-audit, **not** an independent third-party audit). For each threat-model row the relevant code was re-read, then attacked with hostile tests and fuzzers. Findings below were all fixed unless listed under *Accepted* or *Open*.

## Method

1. Walk threat rows T1–T15; list the code that is supposed to enforce each mitigation.
2. Re-read that code looking for ways the claim could be false (not for style).
3. Write a failing test or fuzzer for every suspicion; fix; keep the test.
4. Run 12 fuzz targets (`scripts/fuzz.sh`, millions of executions each, no open crashes) and the hostile-repo, hostile-discovery and hostile-request suites.
5. Measure the sandbox (`systemd-analyze security`: 6.7 → 1.6) and run daemon and tray under the full restriction set.

## Findings (all fixed)

| # | Severity | Finding | Fix | Regression test |
|---|---|---|---|---|
| F1 | **Medium** | A token could be offered to git for a **cleartext `http://`** remote on a non-loopback host (daemon built the credential from the URL scheme without checking, and the askpass helper only compared hosts). A malicious or misconfigured provider/redirect could therefore have captured it. | Both the daemon (`credFor`) and the askpass helper (the last line of defence) now refuse anything but `https`, or `http` to loopback. | `TestRun_NeverDiscloses_InCleartextToRemoteHosts`, `FuzzRun_NeverPanicsAndNeverLeaksToOtherHosts`, `TestCredFor_UsesTheProvidersUsername` |
| F2 | **Medium** | Clone URLs returned by a provider API were used unvalidated: `file:///…`, `ext::` remote helpers, `http://`, `git://`, local paths or option-looking strings (`-oProxyCommand=…`) were all accepted. | `provider.ValidCloneURL` allows only `https`, `ssh` and scp-style ssh; applied at discovery (repo skipped and reported) and again before cloning. | `TestValidCloneURL`, `FuzzValidCloneURL_NeverPanics`, `TestDaemon_HostileDiscovery_…` |
| F3 | Medium | SEC-6 promised "refuse symlink escapes from the clone root", but a symlinked namespace directory (`<root>/github/acme → /elsewhere`) would have redirected a clone. | `provider.CheckNoSymlinks` before every clone; the repo is flagged *needs attention* (`unsafe-path`). | `TestCheckNoSymlinks`, `TestDaemon_SymlinkedNamespace_CloneRefused` |
| F4 | Low | A hostile or buggy `Retry-After: 99999999999` / far-future reset header could park a host indefinitely. | Server-supplied waits capped at 24 h. | `TestRelease_HostileRetryAfterAndReset_AreCapped`, `FuzzRelease_HeaderParsing_…` |
| F5 | Low | Validation errors echoed the offending URL, including an embedded password. Found by the test written for F2. | URLs are redacted, stripped of control characters and truncated before they enter any message. | `TestValidCloneURL` (asserts no `pw@` in errors) |
| F6 | Medium (correctness) | The unsafe-repo-config check rejected **legitimate Git LFS repositories** (`git lfs install --local` writes `filter.lfs.*`), so they could never sync. | Exact-match allow-list of what `git lfs install` writes; any other value, filter name or path is still rejected. | `TestCheckSafeConfig_StandardLFSAccepted_ImpostorsRejected` |
| F7 | Low | The config file was read without a size bound. | 1 MiB cap. | `TestLoad_OversizedFile_Refused`, `FuzzParse_NeverPanics` |
| F8 | Medium | (found in M3a) The HTTP ETag cache was keyed by URL only, so two accounts on one host could receive each other's cached responses. | Cache key includes a hash of the credential. | `TestDo_ETagCache_IsolatedPerCredential` |
| F9 | Low | TLS certificate failures were retried with backoff and counted against the host's health. | They fail immediately with a hint to configure `ca_file`, and never penalise the host. | `TestDo_CertificateError_FailsFast_NotRetried_NoHostPenalty` |
| F10 | Info | Local-path remotes were throttled at 1 request/second although no server needs protecting. | `local` bypasses the host limiter (the worker pool still bounds it). | daemon suite |

## Threat-model status after review

| Threat | Status |
|---|---|
| T1/T2 loopback UI (CSRF, DNS rebinding, local users) | Mitigated: Host and Origin checks, one-time login code, `SameSite=Strict` session, CSRF token, strict CSP, machine API only with the control token. Fuzzed (`FuzzUI_HostileRequests_…`). |
| T3 token leakage | Mitigated: `secrets.Token` redaction, scrubbing logger writer, per-credential askpass (host-bound, https-only), scrubbed git errors/args, token never in argv/URLs/config/DB. Residual: readable by the same user via `/proc/<git>/environ` while a git call runs (ADR-0015). |
| T4 over-scoped tokens | Mitigated by warnings (classic `repo`, GitLab `api`) and documented minimal scopes. |
| T5 MITM | Mitigated: verification always on, TLS ≥ 1.2, private CAs via explicit `ca_file`, redirects to http refused. |
| T6 hostile repo executes code | Mitigated: hooks and fsmonitor off by environment, remote helpers off, submodule recursion off, unsafe repo-local config refused (LFS exception is exact-match). |
| T7 path traversal / symlinks | Mitigated: strict segment validation (fuzzed), `CheckNoSymlinks`, `LocalPath` never escapes root (fuzzed). |
| T8 data loss | Mitigated: fail-closed predicate verified exhaustively, write-ahead audit, race-safe deletes, trash refs, UI cannot name branches to delete. |
| T9 provider abuse | Mitigated: per-host limiter, caps on server-supplied waits, floors on intervals. |
| T10/T11 supply chain | Partly mitigated: pinned modules, `govulncheck`, reproducible releases checked in CI, keyless signatures and provenance (pipeline not yet exercised on a real tag). GitHub Actions are pinned by commit SHA (Dependabot keeps them current). |
| T12 local tampering by same-privilege malware | Accepted (out of scope), config strictly validated. |
| T14 resource exhaustion | Mitigated: output, body, config and file size caps, bounded caches and worker pool; measured in [PERFORMANCE](PERFORMANCE.md). |

## Accepted risks and known limitations

- Same-user malware can read the keychain session, `/proc/<pid>/environ`, the runtime control token and the session; repo-keeper does not defend against an attacker with your privileges.
- The session cookie cannot be `Secure` because the origin is `http://127.0.0.1`.
- Passphrase-encrypted secret file uses PBKDF2-HMAC-SHA256 (600 000 iterations) with AES-256-GCM so it needs no third-party code; Argon2id would resist GPUs better. Prefer the OS keychain, agenix/sops files, or systemd credentials where available.
- Case-insensitive filesystems (macOS/Windows) are not handled for repository path collisions; Linux is the supported platform.
- Provider terms and limits in [PROVIDERS](PROVIDERS.md) still carry *last verified: TBD* and need a human check against the live documents.
- This was a self-review. An independent review before a 1.0 announcement is recommended.

## Open items

- ~~Pin GitHub Actions by commit SHA~~ done: every action is pinned to a commit with its version in a comment; Dependabot proposes updates.
- Run the first signed release and verify the documented `cosign` and `gh attestation` commands end to end.
- Schedule the fuzzers in CI (`.github/workflows/fuzz.yml`, added in this milestone) and triage what they find.
- **Review the implemented `update` command** (ADR-0021/0022) against T11: the cosign invocation and exact-identity pin, the asset-URL restriction, archive extraction, the hard-link/rename replacement and rollback, and the isolation test (`internal/update/isolation_test.go`). Built and tested, including against the real signed releases, but not yet independently reviewed.
