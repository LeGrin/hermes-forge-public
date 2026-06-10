# Stack Decision

Hermes and Forge are implemented in Go.

Reasons:

- Fast test/build loop.
- Simple HTTP and JSON support in the standard library.
- Good fit for small static service binaries.
- Straightforward local process management for Forge.

The repository is intentionally split into multiple Go modules so Hermes, Forge, and e2e tests remain decoupled.
