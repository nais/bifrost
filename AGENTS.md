# Agent Instructions

## Philosophy

Follow [tiger style](https://tigerstyle.dev/): Prefer obvious code over clever code. Prioritize readability and maintainability.

## Project Structure

- **Go project** using mise for task management
- **mise.toml** is the single source of truth for:
  - Go version and tools
  - Build tasks and checks
  - Version generation
- **GitHub Actions** workflows use mise tasks for all operations

## Coding Guidelines

**Comments:**

- Write inline comments that explain **why**, not what
- Never document obvious code
- Avoid redundant comments

**Documentation:**

- Never create new documentation files
- Update existing docs (README.md) only when necessary
- Keep updates minimal and relevant

**Communication:**

- Be concise in all responses
- No lengthy summaries after coding sessions
- Skip unnecessary explanations of what was changed

## Workflow

**After every coding session:**

```bash
mise run all
```

This runs: tidy, fmt, lint, vet, check, test, build

**Before committing:**

- Ensure all checks pass
- Run `mise run ratchet-check` to verify actions are pinned

## Architecture

- **cmd/** - CLI commands (cobra)
- **pkg/domain/** - Core business logic
- **pkg/application/** - Use cases
- **pkg/infrastructure/** - External dependencies (K8s, GitHub, CloudSQL)
- **pkg/api/** - HTTP handlers (gin)

## Key Patterns

- Use context for cancellation and timeouts
- Handle errors explicitly, don't ignore them
- Validate inputs at boundaries
- Keep functions small and focused
- Prefer composition over inheritance
