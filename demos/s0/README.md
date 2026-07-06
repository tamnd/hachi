# demos/s0: talk to codex in a real repo

This is the S0 gate as a script you can watch: hachi boots in a fresh
git repo, a real codex turn reads a file and answers, and the session
journal keeps the evidence.

```sh
demos/s0/demo.sh
```

It needs `expect`, a `codex` binary on PATH, and a logged-in codex
account. No mocks: the turn spends real tokens (a handful; the task is
reading a five-line file).

The script builds hachi from the checkout, or runs `HACHI_BIN` if you
set one. It points `HACHI_HOME` at a throwaway hive so your own
sessions are never touched, then checks four things by name:

1. the welcome screen renders
2. the answer shows up in the live transcript
3. the turn winds down to idle
4. the journal holds the prompt, the answer, and the resume handle

Pass or fail, the throwaway journal, the raw pty transcript, and the
version pins land under `demos/s0/artifacts/<timestamp>/` (or
`DEMO_OUT`), because a gate result without its evidence is a rumor.

Exit codes: 0 pass, 1 a named check failed, 3 infra (expect or codex
missing, codex logged out) so an upstream wobble never reads as a
hachi regression.
