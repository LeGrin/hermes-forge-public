# Envelope Spec v0

Envelope is the canonical unit of transportable work.

## Minimal JSON fields

```json
{
  "id": "env-demo",
  "created_by": "automation",
  "domain": "example",
  "project": "demo",
  "target_node": "local-forge",
  "target_executor": "example-executor",
  "task_title": "walking skeleton demo",
  "task_goal": "prove transport end-to-end"
}
```

## Status values

- `created`
- `delivered`
- `read`
- `in_progress`
- `paused`
- `blocked`
- `awaiting_confirm`
- `done`
- `failed`
- `lost`
