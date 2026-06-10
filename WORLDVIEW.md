# Hermes / Forge — Public Worldview

## Hermes

Hermes is a transport node, delivery authority, and live registry for task envelopes. It should know what work exists, where it is targeted, and which status has been confirmed.

Hermes does not execute task content itself.

## Forge

Forge is a local execution companion. It accepts deliveries and manages local session binding for executor processes.

## Core objects

- Envelope
- Delivery
- Status
- Session
- Executor
- Project

## Design philosophy

- Deterministic transport before adaptive reasoning.
- Truthful status before optimistic reporting.
- Small reliable core before broad orchestration.
