# Agent Rules

## Scope and Safety

- Preserve unrelated user changes and keep edits within the requested scope.
- Do not stage, commit, push, merge, create branches or worktrees, or rewrite
  history unless the user explicitly asks.
- Do not decrypt secrets or expose secret values. Keep secret material managed
  through SOPS and reference its declared paths from Nix modules.
- Avoid changing machine hardware, disk, boot, or secret configuration unless it
  is directly required by the task.

## Git Conventions

- Commit messages follow Conventional Commits: `type(scope): subject`.
- Use `git commit -s` so the `Signed-off-by:` trailer is present.
- Use a lowercase imperative subject with no trailing period and a concise,
  meaningful scope such as `app`, `ui`, `tracking`, `carrier`, `auth`, `db`,
  `agents`, or `tooling`.
- Never add a `Co-authored-by:` trailer.
