# HTML Report Format

The architectural review is rendered as a single self-contained HTML file in the OS temp directory.
<!-- bukerov-local-patch: architecture-selfcontained-html -->
No CDN scripts: inline a minimal hand-written CSS layer instead of the Tailwind CDN, and render diagrams as
inline SVG or preformatted ASCII instead of the Mermaid CDN import. Hand-built divs and inline SVG handle the
more editorial visuals (mass diagrams, cross-sections). Mix diagram styles — don't lean on one pattern for
everything, it'll start to look generic.
<!-- /bukerov-local-patch: architecture-selfcontained-html -->

<!-- bukerov-local-patch: architecture-selfcontained-html -->
## Scaffold

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Architecture review — {{repo name}}</title>
    <style>
      /* fully inline layout layer — no CDN scripts, no external fonts/images.
         dashed seam lines, hand-drawn-feeling arrow heads, etc. */
      body { background: #fafaf9; color: #0f172a; font-family: sans-serif; }
      main { max-width: 64rem; margin: 0 auto; padding: 3rem 1.5rem; }
      .seam { stroke-dasharray: 4 4; }
      .leak { stroke: #dc2626; }
      .deep { background: linear-gradient(135deg, #0f172a, #1e293b); }
    </style>
  </head>
  <body>
    <main>
      <header>...</header>
      <section id="candidates">...</section>
      <section id="top-recommendation">...</section>
    </main>
  </body>
</html>
```
<!-- /bukerov-local-patch: architecture-selfcontained-html -->

## Header

Repo name, date, and a compact legend: solid box = module, dashed line = seam, red arrow = leakage, thick dark box = deep module. No introduction paragraph — straight into the candidates.

## Candidate card

The diagrams carry the weight. Prose is sparse, plain, and uses the glossary terms (from the `/codebase-design` skill) without ceremony.

Each candidate is one `<article>`:

- **Title** — short, names the deepening (e.g. "Collapse the Order intake pipeline").
- **Badge row** — recommendation strength (`Strong` = emerald, `Worth exploring` = amber, `Speculative` = slate), plus a tag for the dependency category (`in-process`, `local-substitutable`, `ports & adapters`, `mock`).
- **Files** — monospaced list, `font-mono text-sm`.
- **Before / After diagram** — the centrepiece. Two columns, side by side. See patterns below.
- **Problem** — one sentence. What hurts.
- **Solution** — one sentence. What changes.
- **Wins** — bullets, ≤6 words each. e.g. "Tests hit one interface", "Pricing logic stops leaking", "Delete 4 shallow wrappers".
- **ADR callout** (if applicable) — one line in an amber-tinted box.

No paragraphs of explanation. If the diagram needs a paragraph to be understood, redraw the diagram.

## Diagram patterns

Pick the pattern that fits the candidate. Mix them. Don't make every diagram look the same — variety is part of the point.

<!-- bukerov-local-patch: architecture-selfcontained-html -->
### Graph diagrams (the workhorse for dependencies / call flow)

Use an inline SVG graph or a preformatted-text (ASCII) graph when the point is "X calls Y calls Z, and look at
the mess." Wrap it in a plain bordered card so it doesn't feel parachuted in. Colour leakage edges red and the
deep module dark using inline styles. Sequence-style ASCII works well for "before: 6 round-trips; after: 1." If
a Mermaid-quality rendered graph is genuinely needed, say so in the report and note it requires user-approved
local tooling — do not pull Mermaid from a CDN.

```html
<div style="border:1px solid #e2e8f0;border-radius:8px;padding:1rem;background:#fff;">
  <pre style="font-family:monospace;font-size:0.85rem;">
    OrderHandler --> OrderValidator --> OrderRepo --[leak, red]--> PricingClient
  </pre>
</div>
```
<!-- /bukerov-local-patch: architecture-selfcontained-html -->

### Hand-built boxes-and-arrows (when Mermaid's layout fights you)

Modules as `<div>`s with borders and labels. Arrows as inline SVG `<line>` or `<path>` elements positioned absolutely over a relative container. Reach for this when you want the "after" diagram to feel like one thick-bordered deep module with greyed-out internals — Mermaid won't render that with the right weight.

### Cross-section (good for layered shallowness)

Stack horizontal bands (`h-12 border-l-4`) to show layers a call passes through. Before: 6 thin layers each doing nothing. After: 1 thick band labelled with the consolidated responsibility.

### Mass diagram (good for "interface as wide as implementation")

Two rectangles per module — one for interface surface area, one for implementation. Before: interface rectangle is nearly as tall as the implementation rectangle (shallow). After: interface rectangle is short, implementation rectangle is tall (deep).

### Call-graph collapse

Before: a tree of function calls rendered as nested boxes. After: the same tree collapsed into one box, with the now-internal calls shown faded inside it.

## Style guidance

- Lean editorial, not corporate-dashboard. Generous whitespace. Serif optional for headings (`font-serif` works well with stone/slate).
- Colour sparingly: one accent (emerald or indigo) plus red for leakage and amber for warnings.
- Keep diagrams ~320px tall so before/after sits comfortably side by side without scrolling.
- Use `text-xs uppercase tracking-wider` for module labels inside diagrams — they should read as schematic, not as UI.
- <!-- bukerov-local-patch: architecture-selfcontained-html -->No external scripts — the report is fully static, inline CSS/SVG only, no app code, no CDN imports.<!-- /bukerov-local-patch: architecture-selfcontained-html -->

## Top recommendation section

One larger card. Candidate name, one sentence on why, anchor link to its card. That's it.

## Tone

Plain English, concise — but the architectural nouns and verbs come straight from the `/codebase-design` skill. Concision is not an excuse to drift.

**Use exactly:** module, interface, implementation, depth, deep, shallow, seam, adapter, leverage, locality.

**Never substitute:** component, service, unit (for module) · API, signature (for interface) · boundary (for seam) · layer, wrapper (for module, when you mean module).

**Phrasings that fit the style:**

- "Order intake module is shallow — interface nearly matches the implementation."
- "Pricing leaks across the seam."
- "Deepen: one interface, one place to test."
- "Two adapters justify the seam: HTTP in prod, in-memory in tests."

**Wins bullets** name the gain in glossary terms: *"locality: bugs concentrate in one module"*, *"leverage: one interface, N call sites"*, *"interface shrinks; implementation absorbs the wrappers"*. Don't write *"easier to maintain"* or *"cleaner code"* — those terms aren't in the glossary and don't earn their place.

No hedging, no throat-clearing, no "it's worth noting that…". If a sentence could be a bullet, make it a bullet. If a bullet could be cut, cut it. If a term isn't in the `/codebase-design` glossary, reach for one that is before inventing a new one.
