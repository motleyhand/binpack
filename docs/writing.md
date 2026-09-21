# Comments, Docs and PRs

Code is the primary documentation. Comments guide the reader's eye; they don't replace reading the code. Every line
must earn its place.

## The test

Ask of every sentence: **would a competent reader of this code get it wrong, or lose real time, without it?** If not,
cut it.

Start from nothing and add only what passes. Don't start from everything you know and trim. Shortening means dropping
facts, not squeezing words: a dense comment is as bad as a long one.

Passing isn't enough when several things do. Rank them by what it costs to get each one wrong, and keep the one or two
that matter most. Guard against the likely, expensive mistake, not every alternative a reader might think of.

Worth a line:

- A non-obvious reason: a constraint, an external quirk, a business rule the code can't show.
  `// Invoice numbers may contain slashes, which would nest the object in folders.`
- Load-bearing code that looks removable or wrong.
  `// Keep OFFSET 0: it stops Postgres flattening the subquery into a join.`
- A tempting fix that doesn't work: name the constraint, not the story.
  `// Sequential on purpose: the provider allows one concurrent request per account.`
- A contract the signature can't express: ordering, idempotency, what must hold a lock.
- A known edge case, accepted on purpose, that someone would otherwise "fix".
  `// Can briefly exceed the cap; accepted.`

Not worth a line:

- What the code does, restated. Names, types and structure already say it.
- What a config value, annotation or type already says (`lazy: true` needs no "lazy" comment).
- What a better name, a named constant or a unit could say. Change the code instead (`15 * time.Minute`, not
  `900 // 15 minutes`).

## Write for the reader, not the author

You know how you got here; the reader only needs where things are. Leave out:

- **History**: "was", "used to", "now", "no longer", "as before", "replaced", "previously". Describe the code as it
  is. Git has the rest.
- **Investigation**: measurements, benchmarks, query plans, sample counts, alternatives tried. Keep the conclusion,
  and a number only if it explains a constant. The evidence goes in the PR.
- **Proof**: every case that shows the code is safe. State the invariant, and what breaks without it.
- **Citations**: test names, issue numbers, chains of "see X", unless the reader must go there to act.
- **Code that isn't there**: designs you considered and didn't write.

## Shape of a comment

- One line by default. Past three lines needs a reason; a paragraph belongs in a doc, or means the code wants
  restructuring.
- Put it next to the line it explains. A class or function doc says what the thing is for, not how it works.
- State the fact and its consequence in plain words. No hedging, no bold, no "Note that".
- Say it once: not twice in one comment, not again in the inline comment below the docblock.
- Annotations that tooling needs (`@param`, `@throws`, type tags) aren't prose; keep them, but don't pad them.

## Changing and tidying

- Update or delete every comment your change makes wrong, and fix wrong ones you notice: replace the claim with the
  true one, or delete it. A stale comment is worse than none.
- Don't narrate the change ("now does X instead of Y"). Write the comment the code would need had it always been this
  way — usually none.
- When tidying, change only what fails this guide. Rewording that isn't shorter or clearer is churn.
- Temporary code gets a `TODO:` that says when it can go.

## Markdown docs

The test and the reader-first rules apply here too. A doc holds what the code can't show: how the pieces fit across
files, decisions and their reasons, knowledge that was expensive to get (external rules, legal mappings, vendor
quirks), and how to operate and recover the system.

- Describe the current state. No migration narratives. A decision record names the rejected alternative in one line.
- Don't mirror the code: directory trees, field and method lists, config tables that copy a file. They go stale; name
  the file instead. A table mapping outside rules onto the code (a tax classification, a protocol's error codes) is
  the doc's job — keep it.
- Keep the conclusion, not the evidence. Traces, round-trip counts and benchmarks go in the PR.
- One home per fact: where the reader is when they need it. A comment if it's about one spot in the code, a doc if it
  spans files. Link from anywhere else.
- Lead with the point. Cut filler: "It is worth noting", "You might expect", "essentially", "In other words".
- Size a section by how much the reader needs, not by how much you know.
- Agent instructions (CLAUDE.md, skills, memory) are loaded into every session, so every line costs. Each rule gets
  at most a one-line reason; the full why lives in the doc it links.

## PR descriptions and commit messages

The reviewer has the diff. Tell them what it can't show: why.

- Lead with the change and its effect, in a sentence or two.
- A bug fix names the cause in one sentence, at the depth needed to judge the fix, not how you found it.
  ("Worker threads shared one DB session, which isn't thread-safe.")
- Each non-obvious decision gets one line with its reason. Name a rejected alternative only if the reviewer would
  otherwise ask "why not X?".
- Don't walk the diff. Renames, new methods and moved files are visible ("`fetchUser` renamed to `loadUser`" is noise);
  mention one only if it's easy to miss.
- Call out what must not be missed: breaking changes, migrations, drive-by fixes, follow-ups.
- Evidence (measurements, plans, traces) belongs here rather than in code, but only what backs a decision, summarised.
- Follow the repo's PR template; its sections get the same economy.
- Keep the description true to the final code: update it on every push that changes scope.
- Commit messages are a subject line saying what changed. The PR description carries the why.

## Example

Before:

```
// We call the payment provider with a 10s timeout. Previously this used the HTTP client's
// default, which is infinite, and during the March incident a hung connection held a worker
// for 40 minutes. We tried 5s first, but p99 latency for this endpoint is 6.2s (measured over
// 3 days), so 5s failed ~1.1% of legitimate requests; 10s gives headroom. We also considered
// a circuit breaker but decided it's overkill for now. Note that the provider deduplicates on
// the request id, so retrying is safe, which is why we retry up to 3 times below.
// See PaymentClientTest::testRetriesOnTimeout.
paymentClient = httpClient(timeout: 10s, retries: 3)
```

After:

```
// The client's default timeout is infinite. 10s clears the provider's p99 (~6s).
// Retries are safe: the provider deduplicates on request id.
paymentClient = httpClient(timeout: 10s, retries: 3)
```

Gone: the incident (history), the 3-day measurement and the 5s attempt (investigation — the p99 stays because it
explains the number), the circuit breaker (code that isn't there), the test name (citation), "Note that".
