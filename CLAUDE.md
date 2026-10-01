# watgbridge (Clarus fork) — working rules

A WhatsApp ↔ Telegram bridge: every WhatsApp chat is a forum topic in a Telegram supergroup. This is a fork of `akshettrj/watgbridge`. **Work happens on the `local` branch**, and `main` follows upstream. In the Clarus system this program is the **Hub** (see `D:\Dev\cm_website_astro\clarus-agent\docs\ARCHITECTURE.md`). It is being extended to talk to the support Agent over the protocol in `D:\Dev\cm_website_astro\clarus-agent\protocol\README.md`.

The Hub runs on a **Raspberry Pi in the office** (linux/arm64), because WhatsApp distrusts datacenter IPs. One instance runs per market, each with its own config and data directory.

## Build and test

Go 1.25 with CGO (`mattn/go-sqlite3`). Go is installed **inside WSL** (Ubuntu 22.04), not on Windows. Run from PowerShell:

```
wsl -e bash -lc "cd /mnt/d/Dev/watgbridge && go build ./... && go vet ./... && go test ./..."
```

CI (`.github/workflows/`) builds linux/amd64 and linux/arm64 binaries and the Docker image on pushes to `local`. Pushing is done by the owner, never by an agent.

**If you change `go.mod` dependencies,** `nix/pkgs/watgbridge-dev.nix` `vendorHash` goes stale. Say so in your report rather than guessing a hash.

## Layout

- `main.go`: boot
- `state/`: config (`sample_config.yaml` documents every key) and global state
- `database/`: gorm models (`types.go`) and helpers. `ChatThreadPair` maps a WhatsApp chat to a topic. `MsgIdPair` maps messages for replies, edits and revokes.
- `whatsapp/handlers.go`: WhatsApp → Telegram
- `telegram/handlers.go`: Telegram → WhatsApp and the bot commands
- `utils/`: conversion helpers for both sides
- `modules/`: handler-group plumbing for optional modules

## Rules

- **Database changes must migrate existing rows.** A live Pi install has real `ChatThreadPair` / `MsgIdPair` data. A migration has to be idempotent and safe to run on a DB that already has it. Tests must cover an already-populated DB, not just an empty one.
- **New Agent-link code lives in its own package** (`agentlink/`), with a narrow interface into the existing handlers. Keep the diff in `whatsapp/handlers.go` and `telegram/handlers.go` minimal and easy to read. This is a fork, and upstream merges have to stay possible.
- **Protocol fixtures:** `agentlink/testdata/` is a copy of `clarus-agent/protocol/fixtures/`. Tests must decode every fixture into the Go structs and re-encode them without losing anything. Never edit the copies here by hand. Shapes change in clarus-agent first.
- **Never forward `/ai_*` messages to WhatsApp.** They become `control` events.
- Telegram ids that cross the protocol are strings.
- Commits use conventional style, matching history: `feat(agentlink): …`, `fix(lid): …`.

## Never

- Commit, push, or create branches, worktrees, symlinks or junctions.
- Pair a real WhatsApp account, or run against a real bot token or config, unless the brief says so. Tests use fakes.
- Touch a live database or config. Those live on the Pi, not here.
- Add legal or compliance features or caveats.
