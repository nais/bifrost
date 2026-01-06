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

**Available mise tasks:**

```bash
# Complete workflow (recommended after changes)
mise run all              # Runs: tidy, fmt, generate, lint, vet, check, test, build

# Code generation
mise run generate         # Generate server and client code from OpenAPI spec
mise run openapi          # Generate server code only
mise run openapi-client   # Generate client code only

# Code quality
mise run fmt              # Format Go code
mise run fmt-check        # Check Go code formatting
mise run lint             # Run golangci-lint
mise run vet              # Run go vet
mise run check            # Run staticcheck and govulncheck

# Testing
mise run test             # Run tests
mise run test-race        # Run tests with race detector
mise run test-coverage    # Run tests with coverage report

# Dependencies
mise run tidy             # Tidy and verify go.mod
mise run tidy-check       # Check if go.mod is tidy

# Building
mise run build            # Build binary
mise run start            # Start the application

# GitHub Actions
mise run ratchet-check    # Verify actions are pinned
mise run ratchet-pin      # Pin actions to commit SHAs
mise run ratchet-update   # Update pinned actions
```

**After every coding session:**

```bash
mise run all
```

**Before committing:**

- Ensure all checks pass
- Run `mise run ratchet-check` if modifying workflows

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

## API Development Patterns

**Verify API-CRD Alignment:**

- Always check that API responses match the actual Kubernetes CRD structure
- Use `github_repo` tool to inspect upstream CRD definitions
- Don't assume existing API mappings are correct - verify against source

**Backwards Compatibility:**

- When fixing incorrect API fields, maintain old field names with deprecation markers
- Map deprecated fields to correct new values where semantically equivalent
- Fields with no real data source can return empty strings with clear deprecation notices
- Use OpenAPI/swagger deprecation comments for API documentation

**Domain Model Changes:**

When modifying domain models that represent CRDs:

1. Update domain struct in `pkg/domain/<resource>/`
2. Update repository conversion in `pkg/infrastructure/kubernetes/`
3. Update API handlers and response structs in `pkg/api/http/<version>/handlers/`
4. Update all tests (search for struct literal patterns)
5. Update helper functions that depend on changed fields
6. Run `mise run all` to verify all changes

**Container Image Handling:**

- Extract versions from image strings using helper functions (e.g., `strings.LastIndex` for tag parsing)
- Pattern: `image:tag` where tag is everything after the last `:`

## Testing Patterns

**After Domain Model Changes:**

- Search for struct literal initialization patterns: `grep -n "<StructName>{"`
- Update test fixtures to match new field structure
- Verify mock repository implementations return correct field types
- Run tests frequently during migration to catch issues early
