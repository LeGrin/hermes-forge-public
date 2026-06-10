# Design Decisions

## 1. Split transport from execution

**Decision:** Hermes is the transport/status service; Forge is the local execution gateway.

**Reasoning:** Reliable handoff requires a durable authority for work state, while execution needs local machine context. Splitting them keeps the transport deterministic and the gateway replaceable.

## 2. Use Go for the walking skeleton

**Decision:** Implement the public core in Go modules.

**Reasoning:** Go gives simple deployment, strong typing, straightforward HTTP servers, fast tests, and a small operational footprint.

## 3. Use SQLite for durable local state

**Decision:** Store envelopes, sessions, keys, and related state in SQLite.

**Reasoning:** SQLite is enough for a local/public walking skeleton and makes state-machine behavior easy to test without external services.

## 4. Make status explicit

**Decision:** Track named statuses instead of inferring truth from process logs.

**Reasoning:** Human/agent handoff needs auditable transitions: created, delivered, read, in progress, blocked, done, failed, or lost.

## 5. Keep the public mirror sanitized

**Decision:** Public docs and examples use placeholders and exclude private operations artifacts.

**Reasoning:** The public repo should demonstrate engineering quality without exposing deployment details, secrets, or personal machine context.
