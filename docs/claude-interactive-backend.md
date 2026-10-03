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
Finally, it waits for a pasted prompt to become visible in Claude Code's
bottom input editor before sending the synthetic Enter, then retries Enter
only while that bottom prompt still contains text. This avoids a race where a
large/multiline paste is still being materialized when Enter is sent, leaving
the entire request sitting unsubmitted at `❯` indefinitely. The race was
observed on Claude Code 2.1.287.

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
- inline base64 image prompts, delivered through a private Magpie MCP image
  tool without enabling Claude Code's unrestricted built-in `Read` tool;
- transcript rows where thinking and visible text share one message id;
- cleanup of detached tmux/Claude children after one-off requests;
- restoring an outer client session to the same inner Claude session after a
  clean Magpie process restart;
- changing effort between turns (for example `max` -> `high`) without
  discarding the inner Claude session.

## Session continuity and prompt-cache reuse

The caller's session id (`X-Magpie-Session` or the caller's native session
header) is not the same id as the real Claude Code session inside the PTY.
The interactive backend therefore persists a hashed outer-session key to the
inner Claude session id in `claude-interactive-sessions.json` under Magpie's
config directory. The record also stores the hash of the last completed
assistant checkpoint and the request-context shape.

On a clean gateway restart, a later request whose history still contains that
exact checkpoint is resumed with `--resume <inner-session-id>` and only the
messages after the checkpoint are pasted into Claude. If the history was
rewritten, or the request context no longer matches, the mapping is ignored
rather than risking duplicated context.

Interactive work uses one stable directory per outer conversation under the
system temp directory. This is necessary because Claude Code indexes persisted
sessions by project/cwd. The path is deterministic from a hashed outer-session
key, is rejected if it is not a real directory or is writable by other users,
and is probed for writability before use. Keeping it outside the home also
avoids Claude Code adding an unrelated dotfiles repository's git status to the
system prompt, which would both leak unrelated paths and invalidate the prompt
cache. An earlier prototype used a fresh `/tmp/magpie-claude-*` on every run;
that made a persisted inner session id impossible to resume after a gateway
process restart.

This is complementary to #626 rather than a replacement for it. #626 gives
headless subscription runs one safe shared temp cwd so independent `claude -p`
runs can reuse the common prompt prefix. The interactive backend cannot use one
account-wide cwd for continuity: `--resume` is project/cwd scoped and each
outer conversation must retain its own inner Claude session. Both therefore
want stable temp paths, but at different granularities (shared for headless,
per outer session for interactive).

Before each resumed turn the persisted record is marked dirty. It becomes
clean again only after a complete assistant turn is committed. A client or
process abort in the middle of a turn therefore cannot silently resume a
partially advanced inner transcript on the next request.

This directly affects Anthropic prompt caching. In a VPS smoke test with Haiku
4.5, the first turn created 6,474 cache tokens. After restarting Magpie, the
next turn restored the same inner Claude session and reported 6,474 cache-read
tokens with only 100 new cache-write tokens. In the broken design, an
inner-session reset rewrote the large prefix instead.

## Long-thinking keepalive

Interactive Opus turns can stay inside the TUI for more than five minutes
without appending a transcript row. Magpie's generic relay deliberately stops
synthetic keepalives after five minutes of total provider silence, so Claude
Desktop can otherwise cancel a healthy interactive turn at roughly the same
boundary.

The interactive backend therefore emits an internal liveness event every 15
seconds while its bridge process is alive. The event has no model/content
semantics; `relay` translates it into the downstream protocol's normal
keepalive (`ping` for Anthropic). This preserves the generic five-minute
stuck-provider protection: only the PTY backend, whose child process Magpie
directly owns, is allowed to prove continued liveness.

The first liveness event may also open a streaming response before the model's
first transcript row. Fast quota/auth errors still retain ordinary HTTP error
status because the first heartbeat is delayed; once a genuinely long-running
interactive turn has been acknowledged as alive, any later failure is reported
inside the already-open stream.

Deliberately conservative behavior:

- image URLs are still rejected explicitly; the interactive image path currently
  supports inline base64 images, which covers screenshots and pasted images from
  the tested clients.

## Rollback

Unset `MAGPIE_CLAUDE_INTERACTIVE_BRIDGE` and restart Magpie to return to the
original headless backend. Replacing the custom Magpie binary is not required
for that immediate rollback.
