# Security Model

This repository is a sanitized public mirror. Its security model has two parts: runtime boundaries and publication safety.

## Runtime boundaries

- Hermes stores and reports task state; it does not execute task content.
- Forge owns local execution binding and is expected to run on a trusted local machine.
- API calls use the `X-Hermes-Key` header when a keystore is configured.
- Secrets are passed through environment variables, not hardcoded into prompts or command arguments.
- Public health and static dashboard/icon routes are intentionally unauthenticated; state-changing routes require the configured key model.

## Public-safety sanitization

The public mirror must not contain:

- real `HERMES_KEY` values or production API-key tokens;
- private IP addresses;
- production URLs;
- personal local filesystem paths;
- databases, logs, generated binaries, or deployment manifests;
- private runbooks or operator-only procedures.

Use these placeholders in public docs and examples:

```text
HERMES_KEY=replace-with-local-development-key
HERMES_URL=http://127.0.0.1:8081
FORGE_URL=http://127.0.0.1:8090
```

## Threat model for this snapshot

The public repo is intended for code review and local tests. It does not claim to be a hosted production deployment. The main risks are accidental disclosure and misleading demos. The mirror process therefore favors omission, placeholders, and local-only proof.

## Reviewer checklist

- Search for secret-shaped values before publishing.
- Verify docs use loopback URLs only.
- Confirm CI runs tests without private services.
- Keep private operational runbooks out of the mirror.
