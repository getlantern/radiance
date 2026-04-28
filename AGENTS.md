- Telemetry attributes: follow rules in https://github.com/getlantern/semconv/blob/main/AGENTS.md

## Code Comments

**Default: no comment.** Only add one if a specific *why* is load-bearing — invariant, concurrency guarantee, error condition, zero-value behavior, non-obvious caller contract, or a constraint that would surprise the reader. Aesthetic "this section is well-documented" comments are noise.

Before writing any comment, run this checklist on the proposed text. If any answer is yes, delete or rewrite:

1. Does it restate the identifier name or signature? (`// Foo does foo`, `// updateX manages X across Y`)
2. Does it narrate what the visible next line does? (`// Cancel any existing listener` immediately above `cancel()`)
3. Does it open with a generic lifecycle/management preamble before getting to the point? (`// manages the lifecycle of...`, `// handles the X for Y`)
4. Does it reference tickets, coworkers, sibling files, commit SHAs, or other code locations? Those belong in the commit message / PR description — they rot in source.
5. Does it describe the mechanism instead of the contract? (`authenticates via peer credentials over a Unix socket` vs. `authenticates each connection`)

Lead with the *why*, not a summary of the function. If the only thing you can write is a summary, the comment isn't needed.

Examples:

```go
// BAD — restates name, generic preamble, narrates the code
// updateURLTestListener manages the lifecycle of the URL test result listener
// across VPN status changes. Connected always re-attaches (canceling any
// existing listener) so a stale event still leaves the listener bound to
// the live storage.

// GOOD — leads with the trap, no narration
// Status events are dispatched in unordered goroutines, so reacting to
// intermediate statuses risks a stale handler tearing down a listener
// a concurrent Connected handler just attached. Only Connected (which
// re-attaches unconditionally) and terminal-down statuses are acted on.
```

```go
// BAD — narrates the next line
// Cancel any in-flight offline tests and wait for them to finish.
c.offlineTestCancel()
<-done

// GOOD — no comment; the names already say it
c.offlineTestCancel()
<-done
```

```go
// BAD — references ticket and coworker
// Per Freshdesk #172640 (reported by Alice), saveServers held the lock
// for 1+ minute. We now release access before disk I/O.

// GOOD — states the invariant; the ticket lives in git history
// access is released before disk I/O so a slow write can't starve readers.
```

Before writing an inline comment, consider whether a doc comment on the enclosing function or type would make it unnecessary. Prefer documenting contracts at the declaration over explaining implementation details inline.

TODO comments must state *what* needs to happen and *why* it isn't done now. `TODO: ???` is not actionable — either resolve it or remove it.

## Comment Verification

After any edit that adds or modifies a comment, you MUST spawn a code-reviewer subagent with the diff before declaring the task done. The subagent applies the Code Comments checklist above and reports violations. Fix the violations and re-spawn until the subagent reports none.

You MUST NOT skip this by self-reviewing the diff. The point of the subagent is to review without the generation bias of the Claude that wrote the comment — a self-review by the writer is a known failure mode and does not satisfy this step.

## Go Doc Comments

- When a doc comment is warranted on an exported identifier, start it with the identifier's name and use complete sentences: `// Foo does X.` The first sentence is the summary shown by `go doc` and pkg.go.dev.
- Package comments: one per package, above the `package` clause (conventionally in `doc.go` for larger packages), starting with `// Package foo ...`.
- Formatting (gofmt-aware since Go 1.19): blank lines separate paragraphs; indented lines render as code blocks; lines starting with `-`, `*`, or `1.` render as lists; `[Name]` links to other symbols; `# Heading` renders as a heading. Avoid HTML and manual wrapping.
