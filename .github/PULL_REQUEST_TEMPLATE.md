<!--
Delete any section that genuinely doesn't apply (e.g. Safety impact for a
docs-only change). Don't leave a section in with "N/A" typed into it — either
it matters and gets answered, or it doesn't and gets removed.
-->

## Summary

What changed, and why. One or two sentences — the "why" matters more than the
"what", since the diff already shows what changed.

## Type of change

- [ ] Feature
- [ ] Fix
- [ ] Refactor (no behaviour change)
- [ ] Docs
- [ ] Chore / tooling

## Changes

- 
- 

## Testing

How you know this works. Prefer specifics over "tested it":

- [ ] `npm run ci` passes (Biome + typecheck + tests)
- [ ] New behaviour has test coverage, or a note on why it doesn't
- [ ] Manually verified in the browser / against a running harness (describe what you did)

## Safety impact

Required whenever this PR touches `apps/mcp-server/src/tools/**`,
`agent/sentinel-agent.agent.json`, or anything else that governs what the agent
is allowed to do without approval.

- [ ] Every new or changed tool declares a `risk` in `defineTool()` and the
      annotation matches it (`registry.test.ts` covers this — confirm it's green)
- [ ] Any tool that writes or changes production state is `write` or
      `destructive`, not left unannotated
- [ ] `require_approval_for_tools` in the agent spec still names every
      production-mutating tool literally, not just by tag
- [ ] No credential, token, or key is logged, returned in a tool response, or
      committed anywhere in this diff

## Qodo Code Review Evidence

Link the review, and say what it found and what you did about it — not just
that it ran.

- Review: <!-- link to the Qodo comment/thread on this PR -->
- Findings addressed: <!-- what was fixed, or why a finding was dismissed -->

## Checklist

- [ ] No secrets, API keys, or personal data anywhere in the diff
- [ ] Docs updated if this changes setup, behaviour, or the safety model
- [ ] Commit messages explain *why*, not just *what*
