# Security policy

## Reporting a vulnerability

Please report security issues privately via [GitHub Security Advisories](https://github.com/KlyneChrysler/potent-prod/security/advisories/new). Do not file a public issue. We aim to acknowledge reports within 3 business days and ship a patch within 14 days for critical issues.

If GitHub Security Advisories is unavailable for any reason, email klyne@inventivlabs.io with the subject prefix `[potent-security]`.

Please include:

- A description of the issue, the impact, and (if known) the version affected.
- A minimal reproduction or proof-of-concept.
- Any suggested mitigation.

We will credit reporters who request it once a fix has shipped.

## Supported versions

Patches are released against the latest minor version. Pre-1.0 minor versions may receive security backports at the maintainer's discretion.

| Version | Supported |
|---------|-----------|
| 0.1.x   | yes (latest patch) |
| < 0.1   | no        |

## Hardening defaults shipped today

- All proxy modes (`http`, `mcp-http`) bound to non-loopback refuse to start without `POTENT_API_TOKEN`.
- Constant-time bearer token comparison via `crypto/subtle`.
- Inbound request bodies capped at 1 MiB by default (`-max-body-bytes`).
- Distroless `nonroot` container image; `readOnlyRootFilesystem`, dropped capabilities, RuntimeDefault seccomp in the supplied k8s manifest.
- `gosec`, `staticcheck`, and `govulncheck` run on every push and pull request.
- Release artifacts built by GitHub Actions with verified action SHAs and pinned tool versions.

## Known limitations (pre-1.0)

- No OIDC / SSO integration. The proxy and admin auth are static bearer tokens. Enterprise IdP integration is on the v0.2 roadmap.
- No mTLS to upstream. TLS validation uses the system trust store. Custom CA bundles are not yet first-class.
- No PII redaction in the audit log. Cached request and response bodies are stored verbatim. Treat the audit log as sensitive.
- The bolt store is not encrypted at rest. Mount the volume on an encrypted filesystem in untrusted environments.
- Embedder is a hashing-TFIDF approximation. False positives are possible. Tune `semantic_threshold` per tool; raise it on side-effecting tools like `charge_card`.

## Acknowledgements

We will acknowledge security researchers and reporters in this file once an advisory has been published.
