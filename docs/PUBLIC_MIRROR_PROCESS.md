# Public Mirror Process

This repository is a presentation-ready public snapshot, not the private source of operational truth.

## Goals

- Preserve enough code and tests for technical review.
- Remove private infrastructure and operator-only details.
- Keep local verification possible with `make test`.
- Make accidental secret disclosure unlikely.

## Before publishing changes

1. Run the Go tests:

   ```bash
   make test
   ```

2. Search for unsafe values with your normal secret-scanning tool and review any findings before publication.

3. Confirm docs use only placeholders:

   ```text
   replace-with-local-development-key
   http://127.0.0.1:8081
   http://127.0.0.1:8090
   ```

## Excluded from the public mirror

- private runbooks and deployment manifests;
- production URLs and private IPs;
- real API keys or notification tokens;
- local databases, logs, binaries, and generated artifacts;
- personal machine paths.

## CI expectation

GitHub Actions should run only local Go tests for `hermes`, `forge`, and `test/e2e`. CI must not call private services or require production secrets.
