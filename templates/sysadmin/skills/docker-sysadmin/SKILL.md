---
name: docker-sysadmin
description: Diagnose, monitor, reconcile Compose stacks, and manage local Docker containers and resources safely through COGNIX[WS]'s audited Docker v2 sysadmin tools.
---

# Docker Sysadmin Discipline

Use the `docker` extension tools (`inspect`, `operate`, `compose`, `maintain`) instead of raw destructive `docker` shell commands whenever you inspect or operate local containers, Compose stacks, health incidents, or storage resources.

## Operating Procedure

1. **Observe before proposing changes**
   - List stacks and live metrics with `inspect` → `containers`, or inspect one container's mounts, networks, restart count, and exit code with `inspect` → `container`.
   - Read redacted, severity-tagged logs across an entire stack or single container with `inspect` → `logs`.
   - When working alongside the owner, call `inspect` → `open-panel` so the Docker panel opens right beside your conversation.

2. **Diagnose from facts, never guesses**
   - Run `maintain` → `diagnose` on a stack to collect structured `Observation` → `Hypothesis` → `Procedure` findings (OOM kills, crash-loops, failing health probes, stopped services, or sustained memory pressure).
   - Never claim an upstream dependency failed unless logs or probe output explicitly prove it.

3. **Respect supervised execution (`5-minute` plans)**
   - Non-destructive `start` (`operate` → `manage` with `action: "start"`) executes immediately with target locking and post-check verification.
   - Stopping or restarting containers (`operate` → `manage`), stack operations and repair procedures (`maintain` → `plan` + `execute-plan`), single-item unused resource removals (`kind: "resource"`), and Compose deployments (`compose` → `compose-deploy`) create a **5-minute plan** with preflight fingerprints and file one deduplicated Inbox notification so the owner confirms on **Docker ▸ Plans**.
   - Never bypass owner confirmation by running `docker rm -f`, `docker system prune`, `docker volume rm`, or `docker compose down -v` in a shell.

4. **Compose & Storage Safety**
   - Always run `compose` → `compose-preview` before `compose-deploy` to verify resolved services, images, and ports inside the workspace folder.
   - Only unused images, volumes, and custom local networks (`removable: true`, zero running or stopped container consumers) can be scheduled for single-item removal.
