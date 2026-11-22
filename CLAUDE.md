# washhouse: notes for Claude

Read `PLAN.md` (decisions and phases) and `docs/HOT-WATER-SYSTEM.md` (the
cylinders) before changing behaviour. tanktemp's `docs/WIRE.md` and
`docs/METER.md` are normative for what washhouse reads.

## Rules

- **Strict red/green TDD.** Write the failing test, run it and see it fail,
  then make it pass. Report the red as well as the green. For a fix, keep a
  deliberate mutant in mind (or run it) that the new test must fail.
- **Concurrency is tested hard:** anything with goroutines runs under
  `go test -race -count=200` before it is called done.
- **The core is pure.** Core packages never import `os`, `net`, `syscall`,
  MQTT, HTTP or Influx; `boundaries_test.go` enforces it, so add every new
  core package to its list. Time is injected, and the core runs on **event
  time** from message timestamps, never the wall clock.
- **Outside systems sit behind small interfaces** declared where they are
  used, with hand-written fakes and a shared contract test that every
  implementation (real and fake) passes.
- **The meter is the truth for volume.** Draws are segmented from metered
  litres; the probes attribute them and give a cross-check. Never publish a
  probe-only volume as a draw.
- **Config is declared and strictly validated.** Unknown keys are refused,
  numbers that must be integers are checked as integers (yaml.v3 truncates
  4.5 to 4), and no hardware or threshold value lives in code.
- **Goldens are the wire contract.** Never edit one to make a test pass; a
  change starts in `docs/WIRE.md`. Key order is pinned. Unknown is `null`,
  never 0.
- **The OpenAPI spec and README change in the same commit as the behaviour.**
- **Common modules are pinned to tags** in `go.mod`. To try an unreleased
  change in common, use a gitignored `go.work` in this repo, and pin the new
  tag before pushing.
- **Scripts enforce rollout order** (for example, check new config with the
  installed build before switching), and follow the owner's script rules:
  `set -euo pipefail`, idempotent, `--check` first, `--yes` to skip prompts,
  tested before the owner is asked to run them.

## Privacy (this repo is public)

- **No PII:** no real hostnames, IPs, SSIDs, site names, email addresses or
  house details in committed files, commits or PRs. The deploy host and real
  config are gitignored (`deploy/host`, `deploy/config.local.yaml`); the
  broker and tanktemp's `/healthz` URL live in local files under
  `~/washhouse-recordings/`.
- **Draw times are an occupancy pattern.** Fixtures cut from recordings are
  shifted to a fake epoch (`scripts/make-fixture.sh`), and docs never give
  the house's real draw times or timer windows.
- **Recordings stay local**, never committed.

## Process

- **Commits:** ask what date and time each should carry, and set both
  `GIT_AUTHOR_DATE` and `GIT_COMMITTER_DATE`. Builds are stamped with the
  commit hash, so settle dates before deploying.
- tanktemp and statehouse are separate repos with their own sessions. Never
  edit them from here: changes to shared docs go to their session as
  patches, one writer per file.
- Never edit `/boot/firmware/config.txt` on any Pi and never reboot without
  asking.
