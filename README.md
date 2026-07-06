# hachi 蜂

A hive for AI coding agents.

hachi wraps any coding agent (codex today, claude and more next) in a glass hive: you get one conversation in front, and every command, edit, and token the agent spends is visible behind it.
It is a single Go binary with a terminal UI, built on the Unix idea that the pieces should stay small, composable, and inspectable.

## Why

Running agents from a stack of terminal tabs works, but you lose track of what each one is doing, what it changed, and what it cost.
Agent wrappers that hide the underlying tool lock you in and go stale when the tool changes.

hachi takes a different shape:

- **One conversation in front.** Boot it in a repo and start typing, like any agent CLI you already know.
- **Glass walls.** Every shell command, file edit, and token count streams into the transcript as it happens.
- **Any brain.** Agents plug in as drivers behind a registry, database/sql style. Your account, your install, your config; hachi adds observation, sessions, and orchestration on top.
- **Sessions that survive.** Every event is journaled to plain JSONL on disk. Quit, come back, reopen, and the whole transcript replays.

## Install

```sh
go install github.com/tamnd/hachi/cmd/hachi@latest
```

You also need at least one agent installed and logged in.
Today that means [codex](https://github.com/openai/codex):

```sh
codex login
```

## Use

```sh
cd your/project
hachi
```

Type what you want built and press enter.
The agent's reasoning, commands, and edits stream in as cards; the status bar shows elapsed time and token spend.

| Key | Action |
|-----|--------|
| enter | send |
| ctrl+j | newline in the composer |
| esc | stop the running turn |
| ctrl+l | session switcher |
| ctrl+n | new session |
| ctrl+c | quit |

Other commands:

```sh
hachi brains     # list detected agents
hachi --brain codex --dir ~/src/thing
```

## Design

```
tui  ──▶  hive.Service  ──▶  engine  ──▶  adapter (codex, ...)
                               │
                            journal (JSONL on disk)
```

Four interfaces, each four methods or fewer, hold the whole system together: `adapter.Adapter`, `adapter.Stream`, `hive.Service`, and `journal.Journal`.
The TUI is a pure client of `hive.Service` and never touches adapters or the journal; an architecture test enforces that boundary in CI, along with a hard budget on direct dependencies.
The event stream (package `waggle`, after the bee dance) is the one contract everything shares.

Sessions live under `~/.hachi/sessions/<id>/` as `events.jsonl` plus a small `meta.json` (set `HACHI_HOME` to keep them elsewhere).
They are plain files: `grep` them, `jq` them, back them up.

Want to see the whole loop run against a real codex account without touching your own sessions? `demos/s0/demo.sh` drives the binary through a pty, checks the answer and the journal, and archives the evidence.

## Status

Early and moving fast.
Today: codex driver, live transcript, session journal and replay, resume across turns.
Next: richer steering, change review and accept/undo, parallel sessions with a board view, claude and opencode drivers, local models.

## License

MIT
