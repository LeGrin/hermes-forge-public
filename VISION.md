# Hermes / Forge — Public Vision

Hermes exists to make delegated automation work observable and reliable. It receives structured task envelopes, stores them, delivers them to a target gateway, and records truthful status transitions.

Forge is the local gateway. It accepts deliveries from Hermes and is responsible for connecting them to local executor sessions.

The public v0 goal is a small, typed, testable skeleton:

1. Create a task envelope.
2. Store it in Hermes.
3. Deliver it to Forge.
4. Record delivery and execution status.
5. Expose enough API surface for tests and local experiments.
