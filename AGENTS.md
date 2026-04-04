# AGENTS

## Testing Notes

- Do not replace `httptest.NewServer`-based tests just to satisfy the sandbox.
- If a verification command fails because the sandbox blocks local port binding, rerun it with escalated permissions instead.
