# Scope and Proportionality Review

Review whether the implementation is the smallest maintainable solution that
meets the requested outcome and its real safety and compatibility constraints.
This lane owns proportionality; the architecture lane still owns component
boundaries, shared abstractions, consumers, and repetition.

## Scope Control

- Compare every substantial mechanism with the user request, acceptance
  criteria, initiative plan, and explicit non-goals. Flag behavior justified
  only by possibilities outside the supported contract.
- Flag code that reimplements operating-system, language, database, Git,
  framework, deployment-tool, or protocol internals when delegating to the
  owning tool would satisfy the supported contract.
- Require a current consumer, demonstrated failure, documented compatibility
  requirement, or concrete operational need before adding generalized
  frameworks, registries, fallback paths, compatibility shims, or exhaustive
  edge-case handling.
- Check whether reviewer-driven fixes broadened the product contract instead
  of correcting behavior inside the chosen boundary. Prefer recording a clear
  non-goal or residual risk when supporting it would be disproportionate.
- Look for obsolete branch iterations, defensive layers, tests, and
  documentation that can be removed after the final design is chosen. Count
  maintenance surface, concepts, and failure modes, not only lines of code.
- Check that tests are proportional to owned behavior. Do not require the
  application to duplicate exhaustive conformance testing for an upstream tool
  whose public operation is deliberately delegated.

Do not use proportionality to excuse a correctness, security, data-loss, or
compatibility defect inside the explicitly supported boundary. When the
boundary itself is unclear, report the ambiguity and the smallest credible
contract rather than silently choosing a broader one.

Use `Blocking` when scope growth contradicts an explicit user decision,
materially increases delivery or operational risk, or makes the commit series
incoherent. Use `Important` for disproportionate maintenance surface or
speculative generalization with a credible long-term cost. Use `Advisory` for a
smaller simplification whose current cost is limited.
