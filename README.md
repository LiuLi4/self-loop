# self-loop

A **Loop Engineering** skill for [Claude Code](https://claude.com/claude-code): drive autonomous, parallel development from a Feishu (Lark) requirements doc — and keep an issue board in Feishu in sync until everything is green.

You hand it a Feishu document that records, per requirement, the **business flow** and the **APIs involved**. The skill then:

1. **Intake** — pulls the doc, semantically splits it into requirements, and *freezes* a machine-checkable acceptance contract (DoD) for each one.
2. **Build** — fans each requirement out into its own git worktree and implements it in parallel.
3. **Verify** — an *independent* checker re-runs the command-level criteria and adversarially refutes the semantic ones (maker ≠ checker).
4. **Sync** — every issue found is idempotently written back to a Feishu Bitable board with a status.
5. **Loop-until-dry** — repeats until *all* DoD criteria pass and no issue is open (or a round cap is hit, then it stops and hands off to a human).

> Design principle: a loop is only as trustworthy as its **idempotent write-back** and its **convergence guarantee**. Both are pinned down with a tiny bit of deterministic code (the Go bridge + the round cap), not left to model judgement.

## Layout

```
self-loop/
├── skills/self-loop/
│   ├── SKILL.md                 # the skill entrypoint (preflight, env guidance, hands off to the workflow)
│   └── self-loop.workflow.js    # orchestration: pipeline + worktree isolation + loop-until-dry
└── loop-bridge/                 # Feishu Open API bridge CLI (Go stdlib only, no third-party deps)
    ├── main.go                  # doc-dump / issues-list / issue-upsert
    └── main_test.go
```

## Prerequisites

- **Claude Code** with the `Workflow` tool and worktree-capable agents.
- A **Feishu/Lark self-built app** with doc-read + bitable-read/write permissions.
- A **Feishu Bitable** to act as the issue board, with fields:
  `external_key` (single-line text, the idempotency key), `requirement`, `title`, `type`,
  `status`, `severity`, `acceptance_ref`, `evidence`, `updated_round` (number).
- **Go 1.22+** to build `loop-bridge`.

## Install

```bash
# 1. build the bridge
go install github.com/LiuLi4/self-loop/loop-bridge@latest   # or: go build -o ~/bin/loop-bridge ./loop-bridge

# 2. make the skill available to Claude Code
cp -r skills/self-loop ~/.claude/skills/                     # or into <your-repo>/.claude/skills/

# 3. set local env vars (in ~/.zshrc, then `source` / open a new shell)
export FEISHU_APP_ID=cli_xxxxxxxx
export FEISHU_APP_SECRET=xxxxxxxxxxxx
export FEISHU_BITABLE_APP=bascnXXXXXXXX
export FEISHU_BITABLE_TABLE=tblXXXXXXXX
# optional:
# export FEISHU_BASE_URL=https://open.feishu.cn
# export SELF_LOOP_MAX_ROUNDS=6
# export SELF_LOOP_SCOPE_RULE="only touch the billing domain"
```

Credentials are read **only** from the environment by `loop-bridge`; they are never printed, written to disk, or passed on the command line.

## Use

From inside the target git repo, in Claude Code:

```
/self-loop https://<tenant>.feishu.cn/docx/<TOKEN>
```

The skill checks your env vars (and walks you through creating any that are missing), runs a read-only preflight against Feishu, then launches the workflow. It reports back: whether it converged, how many rounds it took, and any issues left open on the board.

## loop-bridge CLI

Purely mechanical Feishu I/O — no requirement parsing (that's done semantically by the workflow's intake agent):

```bash
loop-bridge doc-dump    --doc <document_id>                       # flatten a doc's blocks to text JSON
loop-bridge issues-list --app <app_token> --table <table_id>      # list all board records
loop-bridge issue-upsert --app <app_token> --table <table_id> < records.json   # idempotent upsert by external_key
```

## Guardrails (built into the workflow)

- **Scope guard** (when `SELF_LOOP_SCOPE_RULE` is set): out-of-scope requirements are only flagged as `spec-question`, never implemented.
- **Terminal boundary**: it goes as far as green checks + commit + push + PR + issues closed. It does **not** auto-merge the default branch, deploy to production, or read/write secrets.
- **Frozen DoD**: acceptance criteria are frozen at intake; agents can't relax them to pass.
- **maker ≠ checker**: the verifier is a separate agent that re-runs commands and tries to refute semantic claims.
- **Single writer**: only the workflow's sync stage writes the board, idempotently keyed on `external_key`.
- **Round cap**: stops and hands off if it can't converge within `maxRounds`.

## License

MIT — see [LICENSE](LICENSE).

---

Built with [Claude Code](https://claude.com/claude-code).
