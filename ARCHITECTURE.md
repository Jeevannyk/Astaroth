# Architecture and robustness proof

Status: skeleton. Filled in as each mechanism is built and verified. The design it will describe is in `DESIGN.md`.

## Part 1: Architecture diagram

To be drawn into `diagrams/`. Must show: client API, write path, in-memory index, log writer, commit logic (frame CRC), recovery routine, compaction routine, disk-full guard; the file layout with field sizes; the numbered write ordering; the recovery flow; the locks.

## Part 2: Scenario-to-mechanism proof table

One row per scenario, A1–A7, B1–B6, C1–C6, D1–D4, E1–E5, F1–F4 (32 rows). Columns: scenario, disk state at the kill, mechanism, where in the code, verified by (torture mode + seed, manual steps, or "not verified").

| Scenario | Disk state | Mechanism | Code | Verified by |
|---|---|---|---|---|
| | | | | |

## Part 3: The three guarantees

1. Durability: _to write._
2. Atomicity: _to write._
3. Recoverability: _to write._
