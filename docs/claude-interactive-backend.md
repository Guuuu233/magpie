# Experimental Claude interactive subscription backend

This branch adds an opt-in transport for the Claude Code subscription provider.
The normal `claude -p --input-format stream-json` backend remains the default.

When `MAGPIE_CLAUDE_INTERACTIVE_BRIDGE` points to an executable, Magpie invokes
that wrapper once per turn with `-p --output-format stream-json`. The wrapper is
expected to keep the *real* Claude Code process interactive inside a PTY. The
implementation has been tested with `@desplega.ai/claude-bridge` 0.2.2.

## Why a wrapper is needed

Magpie's existing Claude subscription path uses programmatic/print mode. The
experimental path preserves the existing Magpie routing and MCP callback
machinery while moving only the underlying Claude Code transport to a genuine
interactive TTY.

The real Claude child must not receive `-p`, `--input-format stream-json`,
`--sdk-url`, or other headless-mode switches.

## Requirements

- Claude Code already authenticated on the host that runs Magpie.
- `tmux`.
- Bun.
- `@desplega.ai/claude-bridge` 0.2.2.
- The resume-discovery fix in
  `contrib/claude-bridge-v0.2.2-resume.patch`.

The companion patch is required because resumed Claude sessions append to an
existing transcript. Upstream 0.2.2 can miss that transcript and wait for a
fresh one, which can deadlock resumed MCP tool calls. It also installs
SIGINT/SIGTERM cleanup handlers in print mode; without those handlers Magpie
can terminate the wrapper while leaving its detached tmux/Claude TUI alive.

The patch is generated with zero lines of unified context so the patch file
itself does not contain whitespace-only diff context lines. Apply it with:

```sh
git apply --unidiff-zero contrib/claude-bridge-v0.2.2-resume.patch
```

## Enable

Create an executable wrapper, for example:

```sh
#!/bin/sh
export HOME=/home/magpie
export PATH=/home/magpie/.bun/bin:/home/magpie/.local/bin:/usr/local/bin:/usr/bin:/bin
exec /home/magpie/.bun/bin/bun \
  /home/magpie/.local/share/claude-bridge-magpie/src/cli.ts "$@"
```

Then set:

```text
MAGPIE_CLAUDE_INTERACTIVE_BRIDGE=/home/magpie/.local/bin/claude-bridge-magpie
```

and restart Magpie.

Without this environment variable, Magpie uses its original headless Claude
subscription backend.

## Current scope

Verified:

- text requests;
- Opus 5.5;
- multi-turn resume;
- external MCP tool calls and tool results;
- resumed-session MCP tool calls;
- transcript rows where thinking and visible text share one message id;
- cleanup of detached tmux/Claude children after one-off requests.

Deliberately conservative behavior:

- an effort-level change between turns abandons the interactive run and falls
  back to a fresh conversation path rather than trying to send the headless
  control protocol to the interactive TUI;
- image-specific behavior has not yet been treated as a supported target.

## Rollback

Unset `MAGPIE_CLAUDE_INTERACTIVE_BRIDGE` and restart Magpie to return to the
original headless backend. Replacing the custom Magpie binary is not required
for that immediate rollback.
