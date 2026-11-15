# Zeron design investigation

Core question: which verified Zeron product and architecture choices would make
Marlin a better project coding application?

Snapshot: `zeronsh/zeron@93bed8f3979613ba4f5e87884e9dcf93b2876ff4`, 2026-11-15.
Minimum rounds: 1, because this is a bounded assessment of one repository rather
than an ecosystem survey. Two independent exploration tracks and a fresh
verification pass cover the primary documentation and implementation.

Dimensions:

1. Product/UI: project and session organization, coding actions, tool visibility,
   file/diff/terminal review, information hierarchy.
2. Architecture: engine/UI boundary, agent integrations, project/session identity,
   events, persistence, cancellation/recovery, local/remote model.
3. Applicability: concrete Marlin gaps, what can fit MyGo + the pinned Marlin
   core, and which changes need a larger protocol or backend decision.

Completion criteria:

- Each major recommendation has a primary source and implementation evidence.
- Screenshots, documented claims, source observations, and runtime verification
  are distinguished; source inspection is not described as running Zeron.
- Current and legacy frontend paths are distinguished.
- Findings prioritize user workflows and separate small improvements from
  changes that would replace the existing agent backend.
- A fresh verifier checks coverage and unsupported conclusions.

Scope: read-only source/design investigation. Do not install or run Zeron, sign
in, connect devices, or migrate Marlin to external coding agents.
