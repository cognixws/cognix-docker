# cognix-docker (Docker v2 for COGNIX[WS])

**Docker Sysadmin** extension (`v2.0.0`, `apiVersion: 22`) for **COGNIX[WS]** — the operating platform where people and AI agents work together.

## Highlights

- **Stacks & Compose lifecycle**: groups containers by Compose stack with clickable published port pills, live CPU/memory bars, multiplexed severity-filtered stack logs, and native `docker compose` validation, service diffing, and deployment inside enabled workspaces (volumes preserved).
- **Supervised 5-minute maintenance plans**: stops, restarts, stack repairs, Compose deployments, and single-item unused resource removals create fingerprinted 5-minute plans confirmed through COGNIX[WS]'s native `confirm` dialog (`X-PiCode-Owner-Confirmed`), with deduplicated Inbox notifications when proposed by an agent.
- **Health monitors & structured diagnosis**: 30/60/300s background stack probes with consecutive-sample hysteresis (`2/3/5` bad samples to open, `2` good samples to resolve) and fact-based `Observation → Hypothesis → Procedure` diagnosis.
- **Portable agent capabilities (`picode-tools/1`)**: four capability groups (`inspect`, `operate`, `compose`, `maintain`), the `docker-sysadmin` skill, and the multi-CLI **Docker Sysadmin** agent template work across Claude Code, Codex, Antigravity, OpenCode, Grok, Muse Code, Omp, Pi, and Assistants.
- **Settings ▸ Storage (`storageProvider`)**: reports unused images, build cache, and unused volumes with consumer protection so reviewed items are reclaimed one by one.
- **Fast binary startup**: `bin/run.sh` runs a prebuilt binary when present or compiles `cmd/cognix-docker` once per source digest into `${PICODE_EXT_DATA}/bin/`.

## Install

In COGNIX[WS]: **Extensions → Install extension**, provide this repository URL or local folder, review permissions and capabilities, and click **Install**. Workspaces containing `compose.yml`, `docker-compose.yml`, or `Dockerfile` activate it automatically unless turned off.

## Development

```sh
make test
make build
```

## License

Apache-2.0.
