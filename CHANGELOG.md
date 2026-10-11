# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-10-11

### Added

- Agent Plugins 1.0.0 support, including MCP-backed lifecycle hooks and OAuth login for MCP servers.
- Filesystem search tool (`search_files`).
- Image generation tool, including OpenRouter generation support.
- Voice mode command with synthesized final chat replies.
- Session-scoped todo planning tool with enforced single `in_progress` task.
- Conversation forks and history labels in the Web UI.
- Cross-platform Web UI installers.
- macOS Seatbelt isolation backend.
- Configurable automatic session pruning with DB size, age and count guards.
- Repeated tool-call loop detection.
- Bounded log rotation via gateway log size/backup settings.
- Configurable low-power profile.
- Opt-in custom channel build tags and optional Seahorse in custom builds.
- `/list plugins` command and a readable `/subagents` task tree.
- Summarization and compaction reporting on channels.
- Pruning of old tool results before context compression.
- Five parallel turns by default.

### Changed

- Exec tool: improved shell guidance, per-run timeout support and float64-only timeout argument.
- Renamed protected read files.
- Refactored cron enable/disable commands and dropped the unused payload kind field.
- Consolidated shared helpers for paths, strings, MIME extension mapping and inline content sanitization.
- DeltaChat channel: safer credentials handling, more configuration options and improved documentation.
- Bumped Go to 1.27.2 and `golang.org/x/net` to v0.60.0.

### Removed

- `/btw` side-question command.
- Compatibility facades, tool path forwarding wrappers and unused CLI version wrappers.
- DeltaChat config keys `password`, `imap_server`, `imap_port`, `smtp_server`, `smtp_port`; `invite_link` was renamed to `join_invite_link`.

### Fixed

- SeaHorse: delete `messages_fts` rows by rowid to avoid O(n^2) clears, batch-load message parts in `GetMessages`, apply `busy_timeout` to every connection and avoid needless `VACUUM`s.
- Session pruning: keep session config on save, never age-prune on an untrustworthy clock, always keep the most recently active session, and prune every agent's session store.
- Agent: invalidate prompt cache when daily notes change, serialize concurrent daily-note appends, and make SeaHorse `Close` wait for the prune loop.
- SubTurn: remove send-on-closed-channel race and finish turns gracefully on natural completion.
- Steering: inject post-response messages only once and let steering interrupt critical loop termination.
- Tools: route `AsyncExecutor` tools through `ExecuteAsync`, avoid logging tool argument values and guard media store reads.
- Plugins: reject package FIFOs before opening, restrict resource reads and MCP header env expansion, and hide plugin host paths from skill prompts.
- Image/voice: pin downloads to public IP addresses, scale response limits with media size and synchronize TTS provider reload.
- MCP: limit inherited stdio environment, allow OAuth store loading on Windows and accept case-insensitive localhost endpoints.
- Authentication: use configured scopes when refreshing OAuth tokens and adapt the Copilot provider to SDK v1.
- Updater: use fork releases for self-update and select the matching arch asset.

[unreleased]: https://github.com/afjcjsbx/picoclaw/compare/v0.4.0...HEAD
[0.5.0]: https://github.com/afjcjsbx/picoclaw/compare/v0.4.0...v0.5.0