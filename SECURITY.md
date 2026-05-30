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
- Every release ships a CycloneDX SBOM per archive and is keyless-signed via cosign + Sigstore.

## Verifying releases

Every published archive has a sibling `.cyclonedx.json` SBOM. The
`checksums.txt` file is signed; verify the signature, then the checksums,
then any individual artifact.

```bash
# 1. download release assets from the GitHub release page
#    (potent_<ver>_<os>_<arch>.tar.gz, checksums.txt, checksums.txt.sig,
#     checksums.txt.pem, and any per-archive .cyclonedx.json files)

# 2. verify the signature on checksums.txt
cosign verify-blob \
    --certificate-identity-regexp 'https://github.com/KlyneChrysler/potent-prod/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --signature checksums.txt.sig \
    --certificate checksums.txt.pem \
    checksums.txt

# 3. verify each archive against the now-trusted checksums
sha256sum -c checksums.txt

# 4. verify the container image signature
cosign verify ghcr.io/klynechrysler/potent:<ver> \
    --certificate-identity-regexp 'https://github.com/KlyneChrysler/potent-prod/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 5. scan the SBOM for known vulnerabilities
grype sbom:./potent_<ver>_linux_amd64.tar.gz.cyclonedx.json
```

## Known limitations (pre-1.0)

- The bolt store is not encrypted at rest. Use Postgres (`-store=postgres`) for encrypted-at-rest deployments, or mount the bolt volume on an encrypted filesystem.
- Built-in `hashing` embedder is a hashing-TFIDF approximation. False positives are possible. For production semantic dedup use `-embed-backend=http` with a real transformer model (sentence-transformers, OpenAI, etc.).
- Audit-log records may contain user data when `redact_request_body` is off for a tool. Treat the audit log as sensitive; the S3 sink supports SSE via the bucket policy.

## Acknowledgements

We will acknowledge security researchers and reporters in this file once an advisory has been published.
