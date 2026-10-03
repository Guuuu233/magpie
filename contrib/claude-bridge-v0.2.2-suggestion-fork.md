# Claude bridge suggestion-fork patch

This is an incremental patch for the already-customized claude-bridge 0.2.2
tree used by Magpie's interactive transport. Apply the existing resume patch
first, then apply:

```sh
git apply --unidiff-zero claude-bridge-v0.2.2-suggestion-fork.patch
```

The patch isolates Claude Desktop's hidden prompt-suggestion fork and filters
the inherited transcript rows copied by `--resume ... --fork-session`.
