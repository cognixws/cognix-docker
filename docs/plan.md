# Docker v2 — Sysadmin Extension Plan & Architecture

`cognix-docker` v2.0.0 completes the migration from COGNIX[WS]'s legacy built-in Docker surface (`ADR-0230` D3/E5) into a self-contained **Docker Sysadmin** extension (`apiVersion: 23`).

## What v2 Owns

1. **Extension-owned SQLite persistence (`storage/migrations/001_sysadmin.sql`)**
   - `docker_locks`: per-target `(endpoint, kind, target)` mutual exclusion across UI and agent operations.
   - `docker_operations`: idempotent, audited single-container lifecycle operations (`actor`, `agent_id`, verified outcome).
   - `docker_plans` & `docker_jobs`: 5-minute TTL maintenance and Compose plans with preflight fingerprints and crash-safe `Recover()` (`running` → `unknown`, never replayed).
   - `docker_confirmations`: 64-hex revision records backing COGNIX[WS]'s native `picode.use("confirm")` dialog (`X-PiCode-Owner-Confirmed`).
   - `docker_monitors` & `docker_incidents`: 30/60/300s stack health sampling with consecutive-sample hysteresis (`2/3/5` bad samples to open, `2` good samples to resolve).
   - `docker_compose_projects`: workspace-scoped Docker Compose stacks validated via fixed-argv `docker compose config --format json` and deployed via `docker compose up -d --remove-orphans` (never `down -v`).

2. **Portable Agent Capabilities (`picode-tools/1`)**
   - `inspect`: `containers`, `container`, `logs`, `resources`, `health`, `open-panel`
   - `operate`: `manage`, `history`
   - `compose`: `compose-preview`, `compose-deploy`
   - `maintain`: `diagnose`, `plan`, `execute-plan`, `jobs`, `monitors`
   - Available across all 8 COGNIX[WS] agent CLIs and Assistants without spawning `go run` per call (`bin/run.sh` caches a compiled binary by source hash).

3. **Event-Driven Sysadmin UI (`ui/containers.html`, `docker.css`, `docker.js`)**
   - Engine `/events` stream pushes live updates via `POST /api/ext/v1/events` (`ext.docker.changed`) with zero polling loops.
   - Surfaces in Apps, Workspace tabs (`contributes.workspace`), Agent side panels (`contributes.panels`), and the Bottom Dock (`contributes.dockPanels`).
   - Ships the multi-CLI **Docker Sysadmin** agent template (`templates/sysadmin`), `docker-sysadmin` skill, and living onboarding guide (`contributes.guides`).
