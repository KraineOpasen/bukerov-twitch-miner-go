# Stage 4 → Stage 5 Implementation Handoff

**Dashboard Visual Design System · KraineOpasen/bukerov-twitch-miner-go · extracted verbatim from the completed Stage 4 report (this session) · verdict READY_FOR_STAGE5_IMPLEMENTATION**

Provenance legend used throughout — every item is tagged with one of:
**[CP]** confirmed repository parity (exists in code today) · **[AD]** approved design (Stage 3/4 decision, to be built) · **[INT]** interpretation (flagged, not confirmed upstream) · **[BE:Bn]** pending backend evidence (render-gated) · **[DEF]** deferred control (DPBA).

## Index

1. Repository and design base
2. Authoritative Stage 3 constraints
3. Visual principles and density model
4. Design-token tables
5. Tailwind mapping and legacy-token compatibility
6. Component inventory C0–C18
7. Exact visual treatment of the 13 S-states
8. Responsive rules (desktop / compact / tablet / mobile)
9. Keyboard, focus, contrast, reduced-motion, screen-reader contract
10. HTMX loading, swap, failure and focus behavior
11. Page-application matrix — all 30 routes
12. Mandatory R17 visual semantics
13. B1–B11 visual gates, DP-C, DPBA restrictions
14. Representative high-fidelity mockups
15. Migration seams and compatibility strategy
16. Stage 5 slices S5-1…S5-10
17. Verification debts and open owner decisions
18. Final acceptance checklist

---

## 1. Repository and design base

- Repo: `KraineOpasen/bukerov-twitch-miner-go`, default branch `main`.
- **Design base SHA: `1cf198aa4257a5f9ba250aec29bf027870f8dad7`** (`origin/main` at Stage 4 close; includes merged PR #147 — R17 backend "abort-and-preserve" + revision-guarded light sync — and PR #148).
- The Stage 3 wireframe report was based at exactly `a87334f6e01f1bf2d7996384304562c2696545d0`. The delta from that base to `1cf198aa...` comprises **PR #147** — the R17 backend progress-provenance and last-known-good sync changes, which §12 renders — and **PR #148**, watcher crash-only recovery documentation, which is not part of the R17 backend implementation.
- Stage 4 produced **no commits**; the design exists only as the report this handoff extracts. Stage 5 work runs under its own task contract on a fresh branch from `main`.
- Fixed stack [CP]: Go `html/template` (embedded via `go:embed` in `internal/web/server.go`) + Tailwind **v4** standalone CLI (`internal/web/static/css/input.css` → `app.css`, `make tailwind`; `@import "tailwindcss"` + `@source "../../../templates"`) + HTMX (`static/js/htmx.min.js`) + ApexCharts (vendored) + i18n RU/EN (`t` template func) + SSE status stream (`/api/miner-status/stream`).
- Fonts [CP]: **vendored variable Inter (100–900) and JetBrains Mono (100–800)** at `internal/web/static/fonts/{inter,jetbrains-mono}.woff2`, declared with `@font-face` + `font-display: swap` in `input.css`. No CDN, no external fonts, ever.
- Theme machinery [CP]: dark is default and paints pre-JS (`:root`); `data-theme="dark|light"` + `data-theme-mode="dark|light|system"` on `<html>`; three-button toggle with `aria-pressed` and meta `theme-color` sync in `base.html`.
- F1 token architecture [CP]: Layer A primitives `--prim-night-*` / `--prim-day-*`; theme-scoped semantic tokens on `:root[data-theme=…]`; `@theme inline` re-points Tailwind color names at the semantic layer.

## 2. Authoritative Stage 3 constraints (binding on Stage 5)

- **IA is frozen**: exactly **7 sections, 30 routes** (full list in §11). No route added, removed, renamed, re-owned.
- **13 UI states** (full treatments §7): S-LOAD, S-READY, S-EMPTY, S-PART, S-STALE, S-UNK, S-DEGR, S-FAIL, S-BLOCK, S-DENY, S-NOBACK, S-SESS, S-DEFER. Invariant: **unknown never converts to healthy/completed/claimed/delivered**.
- **Stage 2 canon** [AD]: (a) exactly **two** watch slots everywhere; an empty slot is a definite rendered state («Слот свободен» + machine reason), never hidden; (b) **full roster mandatory** on the queue page; (c) **/drops/claims is the sole owner** of claim lifecycle — every other page links, never restates; (d) **Save/Cancel/Discard per settings category** — category = dirty boundary, sticky bar, discard dialog on leave, **autosave forbidden**; (e) **account-level sound preferences** (persistence gated [BE:B4]) with **browser fail-open** playback stated in the UI; (f) manual queue/slot controls **deferred** [DEF] (§13).
- Classification vocabulary — **the Stage 4 tags above are the canonical labels used throughout this handoff**; Stage 3's vocabulary [CP from Stage 3] maps onto them one-to-one: `CP` = `CP` (CONFIRMED_PARITY) · `AD` = `AND` (APPROVED_NEW_DESIGN) · `INT` = `DPE` (DESIGN_PROPOSAL_PENDING_EVIDENCE) · `DEF` = `DPBA` (DEFERRED_PENDING_BROKER_AUDIT) · `BE:Bn` = `BD-Bn` (BACKEND_DEPENDENCY n) · `DP-C` = `DP-C` (=DESIGN_PROPOSAL_PENDING_FILLED_GROUP_C_EVIDENCE).
- Stage 4 boundary (§14 of the Stage 3 handoff) — **forbidden in Stage 5 too**: backend/API/schema assumptions (incl. rendering any B1–B11 field as available); React/Next/SPA; CDN; external fonts; autosave; activating DPBA controls; presenting DP-C cards as parity; merging sound-status/sound-config ownership; changing the 7×30 IA; converting unknown to positive states; rendering a missing version field as «у вас последняя версия»; `outline: none` without an accessible replacement.
- Open items carried forward, **not resolved by Stage 4**: owner decision №1 (claims history depth: 90-day window vs unlimited — UI supports both via period filter); gaps Г1 (stage docs not persisted in repo), Г2 (`events_drawer.html` has non-localized EN strings — fix in slice S5-7), Г3 (current Health page mixes config+status — split in S5-5, both parities preserved), Г4 (ROI "three tables" composition = стример/стратегия/исход is [INT]), Г5 (transport for new zones unknown → transport-independent contract, §10).

## 3. Visual principles and density model [AD]

Identity: the existing **night console** (F1) is retained, not redesigned — muted violet-tinted dark surfaces, desaturated purple brand `#8b7fd1`, bright acid hues (`--ui-*`, `--log-*`) reserved exclusively for data semantics.

- **P1 — Provenance is the signature element.** Every live data region carries a **provenance chip** (C0): mono-face capsule with freshness age + source + optional session marker. Unknown is a first-class rendered value, never dressed as zero, green, or "latest".
- **P2 — Quiet chrome, loud data.** Chrome uses only surface/text/border/brand tokens; bright hues only on data semantics; no decorative gradients or icon coloring.
- **P3 — State before decoration.** The 13 S-states are the core components; every component spec includes its S-state behavior.
- **P4 — Icon + text + color, never color alone** (CP precedent).
- **P5 — Fixed geometry.** Skeletons, swaps, state changes never shift layout; every async region has reserved height.
- **P6 — Bilingual first.** Size components against RU copy (~25–35% longer than EN); truncation is explicit + tooltipped.
- **P7 — Restraint.** One shimmer (skeleton), two global live regions, no ambient animation.

**Density model** — fixed per surface class, **never a user preference** (a density preference would need persisted storage = inventing a backend field, forbidden):

| Density | Where | Base font | Row height | Padding |
|---|---|---|---|---|
| **data-dense** | roster/queue, claims, logs, diagnostics journal | 13px, mono values | 32px | cells 12×6px |
| **standard** | cards, forms, settings, event journal, campaign cards | 14px | 40px controls | card 16px (dense card 12px) |
| **reading** | `/help/*`, long banners | 15–16px | — | 72ch max measure |

Tablet/mobile transform dense tables → standard cards (§8); nothing silently removed.

## 4. Design-token tables

Layers: **A** primitives [CP] · **B/C** theme-scoped semantics [CP] · **D** system scales [AD, new] · **E** state tiers [AD, new — aliases only, **zero new hues in Stage 4**].

**Themes** [CP mechanics]: dark = `:root` + `[data-theme="dark"]` (default, pre-JS); light = `[data-theme="light"]` (AA-validated day palette); system = `data-theme-mode="system"` resolving via `prefers-color-scheme`. Rule [AD]: every semantic token must exist in both palettes; templates reference semantics only, never `--prim-*`.

**Existing semantic color families** [CP, reuse as-is]: `--surface-{page,sidebar,card,elevated,input}` · `--text-{primary,secondary,muted,on-accent,on-success,on-danger}` · `--border-{default,strong}` · `--status-{success,warning,danger,info,offline}` · `--focus-ring` · `--chart-{grid,label,series-1..6,annotation-ink-dark,annotation-ink-light}` · `--log-*` (level/hue set) · `--rw-{cpu,mem,disk,net}` · `--ui-*` (semantic accents: online/offline/gain/streak/watch/watching/claim/raid/prediction/queue/refund/roi-pos/roi-neg/warn/other/brand-orange).

**Layer E additions** [AD]:

| Token | Definition | Use |
|---|---|---|
| `--surface-overlay` | black 55% (dark) / 35% (light) | drawer/dialog scrim |
| `--surface-inset` | night-950 / day-950 primitives | log well, code, provenance chip bg |
| `--state-ok` | → `--status-success` | S-READY confirmations |
| `--state-info` | → `--status-info` | S-LOAD, neutral notices |
| `--state-caution` | → `--status-warning` | S-STALE, S-PART, S-SESS |
| `--state-danger` | → `--status-danger` | S-FAIL, S-DENY |
| `--state-neutral` | → `--text-muted` | S-UNK, S-DEFER, S-EMPTY icons |
| `--state-*-bg` | `color-mix(in oklab, <tier> 12%, transparent)` | banner/badge fills |

S-NOBACK has **no token** — it is the absence of rendering.

**Typography** [AD] — Inter = prose, JetBrains Mono = evidence (numbers, IDs, ages, reason-codes, logs):

| Role | Face/weight | Size/line |
|---|---|---|
| `type-h1` | Inter 600 | 22/28 (one per page) |
| `type-h2` | Inter 600 | 17/24 |
| `type-h3` | Inter 600 | 14/20 |
| `type-body` | Inter 400 | 14/20 |
| `type-small` | Inter 400 | 13/18 |
| `type-micro` | Inter 500 | 12/16 (no all-caps in RU; letterspacing 0.02em) |
| `type-data` | Mono 500 | 13/18, `tabular-nums` |
| `type-code` | Mono 400 | 12.5/18 |

All numeric table/KPI cells: `font-variant-numeric: tabular-nums`.

**Spacing** [AD]: Tailwind default 4px scale, not overridden. Contract: page gutter 24/16/12px (desktop/tablet/mobile); card padding 16 (dense 12); section rhythm 16–24; control gap 8; icon–text gap 6; dense cell 12×6.

**Radii** [AD]: `--radius-xs` 4px (chips/badges/provenance) · `--radius-sm` 6px (buttons/inputs) · `--radius-md` 10px (cards/banners) · `--radius-lg` 14px (dialogs/drawer) · `--radius-full` (pills/avatars). Nothing else.

**Borders** [AD]: 1px `--border-default` universal container edge (dark elevation is border-carried); 1px `--border-strong` hover/emphasis; **2px reserved for exactly two uses**: focus ring and active watch-slot edge; 3px left rail in tier color on banners/state blocks.

**Shadows** [AD]: `--shadow-1` `0 2px 8px rgb(0 0 0 / .35)` dark / `.10` light (popovers, toasts, sticky save bar); `--shadow-2` `0 8px 28px rgb(0 0 0 / .50)` dark / `.18` light (dialogs, drawer). Cards have none.

**Z-index** [AD]: `--z-base` 0 · `--z-sticky` 10 (table headers, save bar, toolbar) · `--z-popover` 20 · `--z-rail` 30 (rail flyouts) · `--z-drawer` 40 (+scrim) · `--z-dialog` 50 (+scrim) · `--z-toast` 60. Nothing stacks above toasts.

**Motion** [AD]: `--motion-fast` 80ms (hover/press) · `--motion-base` 160ms (swap fades, accordion, badges) · `--motion-slow` 240ms (drawer/dialog enter; exits 0.7×) · easing `cubic-bezier(0.2, 0, 0, 1)`. Only permitted loop = skeleton shimmer; under `prefers-reduced-motion` shimmer becomes static and all durations → 0ms. **Logs, SSE updates and timed-poll refreshes animate nothing**; the swap fade (§10) applies only to user-initiated, non-log HTMX swaps.

## 5. Tailwind mapping and legacy compatibility

Extend the **existing** `@theme inline` block in `input.css` — no config file, no plugin, no new build step.

| Token group | Exposure | Template usage |
|---|---|---|
| semantic colors incl. Layer E | `@theme inline` color names | `bg-surface-card`, `text-text-muted`, `bg-state-caution-bg`, `border-state-danger` |
| radii | `@theme` `--radius-xs…lg` | `rounded-xs/sm/md/lg` |
| shadows | `@theme` `--shadow-1/2` | `shadow-1`, `shadow-2` |
| fonts | already mapped | `font-sans`, `font-mono` |
| type roles | `@layer components` `.type-h1….type-code` | `class="type-h2"` |
| spacing | Tailwind defaults | `p-4`, `gap-2`, `px-3 py-1.5` |
| z-index | component classes `.z-drawer` etc. → `--z-*` | chrome partials only |
| motion | `.motion-fast/base/slow` presets + global reduced-motion override | interactive elements |
| breakpoints | Tailwind defaults `md` 768 / `lg` 1024 / `xl` 1280 | `xl:` sidebar, `lg:` rail, `<lg` drawer |

**Legacy compatibility rules** [AD]: F1's re-pointed legacy names (`bg-neutral-800`-style aliases) stay valid throughout migration and are deleted **only in S5-10** after a grep proves zero template references. New/migrated templates use semantic utilities exclusively; primitives never appear in templates; both palettes must define every token (build-time grep check for unpaired tokens).


## 6. Component inventory C0–C18

All components are Go template partials (target `templates/components/`) + semantic Tailwind utilities. "Inputs" = template pipeline fields (template interface only — never new backend fields). All obey P4 (icon+text+color) and the §9 focus contract; a11y notes below are additive. Classification given per component; "mechanics [CP]" means the interaction mechanic already exists in `base.html`/partials and is reused.

**C0 — Provenance chip** [AD] *(signature element)*
- Responsibility: freshness + source + session evidence for exactly one data region.
- Inputs: `{AgeLabel, Source, Session bool, Aged bool, Unknown bool}`.
- Anatomy: `--surface-inset` capsule, `rounded-xs`, `type-code`, icon + text.
- Variants: `live` (ok ink, «▲ 12 с назад · SSE»), `aged` (caution ink past the region's staleness threshold), `session` («⚑ сессия», caution), `unknown` (neutral, «неизвестно»).
- S-states: renders S-READY freshness; flips to `aged` under S-STALE; `session` = S-SESS marker; `unknown` = S-UNK freshness.
- A11y: plain visible text (no aria-live, no title-only content); placed card/table top-right; the page header aggregates the oldest region into the global stale dot.
- Non-interactive except when paired with a manual «Обновить» button.

**C1 — State block** [AD]
- Responsibility: the single host for S-EMPTY/PART/STALE/UNK/DEGR/FAIL/BLOCK/DENY/DEFER; skeleton form hosts S-LOAD.
- Inputs: `{State, Message, Cause, Time, ActionLabel, ActionTarget}`.
- Variants: `block` (replaces region content — EMPTY/FAIL/UNK), `strip` (banner above **retained** content — STALE/PART/DEGR; the R17 vehicle), `inline` (one table row / one field).
- Anatomy: fills the region's reserved geometry; 3px left rail in tier color + tier icon + `type-body` message + optional `type-code` cause + optional action.
- A11y: only S-FAIL carries `role="alert"`; all others are plain content.

**C2 — Navigation: sidebar / rail / drawer** [AD structure; drawer + mini-slots mechanics CP]
- Responsibility: the 7-section tree in all three viewport forms; active-route indication.
- Sidebar (`xl`, 260px, `--surface-sidebar`): two-level; sections `type-body` 600, routes `type-small`; active = brand 3px left rail + `--surface-elevated` fill + `aria-current="page"`; section auto-expands for the active route; parity `now-watching` mini-slots pinned below [CP].
- Rail (`lg`, 56px icon-only): labels as popover flyouts (`--z-rail`) on hover **and focus** (the keyboard path); every icon `aria-label`ed.
- Drawer (`<lg`): existing mechanics [CP] — toggle with `aria-expanded`/`aria-controls`, scrim `--surface-overlay` at `--z-drawer`, `--motion-slow`, focus trap, Escape, focus returns to toggle.
- States: default / hover (`--surface-elevated`) / active / focus-visible. Nav items never disable.
- Gate: section count badges (События) render only with [BE:B1] evidence — otherwise absent (S-NOBACK), never «0».

**C3 — Cards** [AD]
- Responsibility: the universal standard-density container.
- Anatomy: `--surface-card`, 1px `--border-default`, `rounded-md`, padding 16 (dense 12); header row = `type-h2` + C0.
- Variants: `kpi` (mono 22px value + label + trend in `--ui-roi-pos/neg`), `panel` (default), `list-card` (target of the table→card transform), `link-card` (whole-card link; hover `--border-strong`).
- S-states: any, via embedded C1. No shadows; nesting max 1 level.

**C4 — Tables (data-dense)** [AD; sort-announcement mechanic CP]
- Responsibility: dense evidence surfaces (roster/queue, claims, status rows, diagnostics journal).
- Anatomy: sticky header (`--z-sticky`, `--surface-card`, `type-micro` secondary ink), `<th scope="col">`; sortable headers = real buttons with `aria-sort` + glyph + polite announcement [CP]; rows 32px, hover `--surface-elevated`, no zebra; numeric cells `type-data` right-aligned; reason-codes `type-code` linking `/help/glossary#code`.
- Missing value: **always** `—` + `aria-label` «нет данных» — never 0 (S-PART rule).
- S-states: row-level via C1 `inline`; table-level via C1 `block`.
- Overflow: `overflow-x-auto` inside the card only — the page never scrolls horizontally. `<lg`: transforms to C3 `list-card`s, status column first.

**C5 — Filters / toolbar** [AD]
- Responsibility: one filter row above tables/journals; deep-linkable filter state.
- Anatomy: search (`--surface-input`), enum select-pills, sort control, view toggle (таблица⇄карточки where Stage 3 grants it); active pill = brand 12% fill + count; «Сбросить» only when ≥1 filter active.
- Behavior: filters serialize to query params (`replaceState`); state persists across responsive transforms.
- Gate rule: a facet whose data lacks backend evidence is **hidden**, not fake-disabled (delivery-channel facet without [BE:B3]).

**C6 — Forms** [AD]
- Responsibility: all settings inputs.
- Anatomy: stacked `label` + control + help (`type-small` muted) + error; controls on `--surface-input`, `rounded-sm`, 1px border; toggles 36×20, brand fill when on, always with visible text state («вкл/выкл»); units inside right edge in `type-code` muted.
- Validation: `--state-danger` ink + `aria-invalid` + `aria-describedby`; category-level error summary on save failure.
- Read-only evidence (e.g. LAN CIDRs on `/settings/system`): mono text + lock icon — never a disabled input.

**C7 — Save/Cancel/Discard bar** [AD; Stage 2 canon — one implementation for all 10 categories]
- Responsibility: the category dirty-state lifecycle. Category = dirty boundary. **Autosave forbidden.**
- Anatomy: hidden while clean; dirty → sticky bottom bar (`--z-sticky`, `--surface-elevated`, top border, `--shadow-1`): «Изменений: N» + category scope | «Отменить» (ghost) + «Сохранить» (primary).
- State cycle: clean (absent) → dirty → saving (`aria-busy`, spinner, «Сохраняем…», primary disabled) → success (bar collapses; toast «Сохранено»; restart-required banner **only** on a [BE:B10] signal — never fabricated) → error (bar persists; C1 strip + Retry above it) → validation-blocked (primary disabled **with visible reason text**).
- Leaving dirty (nav, drawer, back) → C8 discard dialog: «Сохранить / Отменить изменения / Остаться».

**C8 — Dialogs** [AD; `hx-confirm` mechanic CP]
- Responsibility: confirmations and the discard flow.
- Anatomy: `--z-dialog`, scrim, `rounded-lg`, `--shadow-2`, max-w 28rem; title `type-h2`; destructive = danger-primary + explicit consequence sentence.
- Variants: confirm-light (native `hx-confirm`, low stakes), confirm-heavy (Перезапуск⚠ / Стоп⚠⚠ / streamer delete — modal), discard (C7).
- A11y: focus trap, Escape = cancel, initial focus on the least destructive action, focus returns to invoker. Mobile: full-width bottom sheet, same DOM.

**C9 — Banners** [AD; `#health-banner`/`#lifecycle-auth-banner` ids CP]
- Responsibility: page/section-level condition strips + the global stale indicator.
- Anatomy: C1 `strip` with width rules; tier rail + 12% fill; dismissible only if the condition is dismissible (S-STALE strip is not — it clears on freshening).
- Global stale indicator (top bar): caution dot + «данные устарели», links to the oldest region.

**C10 — Badges** [AD; claim-state and reason-code vocabularies CP enums]
- Responsibility: compact status encodings.
- Anatomy: `rounded-xs`, `type-micro`, icon + text, tier 12% fill. Never color-only.
- Sets: channel status (`--ui-online` / `--status-offline` / paused); the **7 claim states** — taken from the code dictionary [CP enums], each mapped to ok/caution/neutral/danger tier + unique icon (set is not invented by design); reason-code badge (mono → glossary anchor); «⚑ сессия» (S-SESS, caution); nav counts ([BE:B1]); «DP-C» outline badge (see C13).

**C11 — Progress** [AD]
- Determinate: 6px track on `--surface-inset`, brand fill, mono percent **beside** (never inside) the bar; drop rows add `x/y мин`.
- Unknown: **no bar** — dash + «прогресс неизвестен» badge (S-UNK ≠ 0%; R17 invariant).
- Indeterminate: thin sliding stripe, **only** for in-flight sync attempts; static under reduced motion.
- Timeline variant (claims): vertical mono-stamped list with tier-colored dots.

**C12 — Slots** [AD geometry; sidebar-mini CP] *(exactly two, everywhere)*
- Responsibility: the two watch slots — the only slot rendering in the product.
- Inputs: `{Streamer?, ChannelStatus, ReasonCode, Uptime, PointsDelta, Active bool, EmptyReason?}`.
- Filled: avatar `rounded-full`, name, status badge, reason chip, uptime mono, points delta in `--ui-gain`; active edge = 2px brand (the only 2px besides focus).
- **Empty slot = definite state**: identical geometry, dashed 1px border, «Слот свободен» + machine reason chip + link to queue — never hidden, never a ghost.
- Variants: sidebar-mini [CP `now-watching`], overview-pair (side-by-side ≥md, stacked below), queue-header. Never a third or fabricated slot.

**C13 — Campaign cards** [AD; policy chips CP; filled layout **DP-C**]
- Responsibility: `/drops/*` campaign presentation.
- Anatomy: header = game art in a **reserved** 64×85 box, name, account-link badge (**only** with [BE:B11] evidence — absent otherwise), policy chip (allow/deny/watch [CP policy decisions]); body = drop rows with C11 + claim links **into** `/drops/claims` (authority rule); footer = C0 (last successful full sync + source).
- R17: the footer chip keeps last-known-good freshness while an attempt-failure strip renders above the list (§12).
- DP-C marking: filled cards carry the `type-micro` outline badge «DP-C: макет по свидетельствам группы C» until evidence upgrades them — visibly provisional, never presented as parity.
- Catalog variant (`/drops/upcoming|past`): progress region **absent**, not zeroed.

**C14 — Charts (ApexCharts)** [AD theming; export CP]
- Colors `--chart-series-1..6`, grid `--chart-grid`, labels `--chart-label` (both themed); no gradients/drop-shadows; tooltip on `--surface-elevated`.
- Every chart pairs with (a) a text summary for screen readers stating ranges and gaps, and (b) its data table / CSV export [CP].
- False zeros suppressed: gaps stay gaps, marked «нет данных за период»; annotations use `--chart-annotation-ink-*`; reduced motion disables chart animation.

**C15 — Logs** [AD restyle; all 9 functions CP]
- Anatomy: `--surface-inset` well, `type-code` 12.5px; level hue applied to the **level token only** — message stays `--text-primary`.
- The 9 CP functions, styled: filter toolbar (C5), search with brand 25% highlight, reconnect state strip, «Копировать», «К новым» floating chip on scroll-pause, 10s-poll C0 chip, scroll-pause indicator, line-limit notice («показаны последние N строк»), wrap toggle.
- Mobile compact reader: 3px level rail + level letter per line; horizontal scroll allowed **inside the well only**.

**C16 — Diagnostics** [AD; snapshot ownership + canary CP]
- Subsystem rows: name + tier icon+text + **C0 per row**.
- Canary/watchdog panels; «Запустить канарейку» = gesture + confirm-light (C8).
- **«Скачать Diagnostic Snapshot»** = primary button; this page is the canonical owner [CP] — help pages only link here.
- Version block: SHA/digest/build-time render **only if present** [BE:B8]; absence renders «данные о сборке недоступны» — never «у вас последняя версия».

**C17 — Toasts and alerts** [AD styling; two-live-region model CP]
- Exactly two global live regions: toast stack (`aria-live="polite"`, `--z-toast`, bottom-right desktop / bottom mobile, `--shadow-1`, 5s auto-dismiss, pause on hover/focus) — **success/neutral only**; lifecycle region (`role="alert"`).
- Errors are **never** toasts — always inline C1 at the failure site.
- Toast anatomy: tier icon + one sentence + at most one action.

**C18 — DPBA passive affordance** [DEF]
- One neutral card on `/overview/queue` **only**; no buttons, menus, or greyed ghosts: «Ручное управление (избранное, переключение слота, harvest, переопределения, перестановка очереди) отложено до аудита брокера» + link to `/help/troubleshooting`.
- Canonical S-DEFER rendering; exactly one instance UI-wide; the five controls appear nowhere else in any form.

## 7. Exact visual treatment of all 13 S-states

Global invariants: fixed geometry (P5); icon+text+color (P4); **unknown never converts to healthy/completed/claimed/delivered**; freshness via C0 unless stated.

| State | Rendering | Icon / text / color | Controls | Freshness / provenance | ARIA / live | Prohibited |
|---|---|---|---|---|---|---|
| **S-LOAD** | skeleton in the region's reserved geometry; shimmer | none (no text flash); neutral | not yet rendered | no chip yet | `aria-busy="true"` on region | layout shift; skeleton for statically-known content |
| **S-READY** | normal content | — | normal | **C0 chip mandatory** on every live region | none beyond content semantics | omitting the chip |
| **S-EMPTY** | C1 `block` | empty-box icon; «Пусто» + why + first step; neutral ink | one CTA link (e.g. «добавьте стримеров» → `/settings/streamers`) | chip may show a fresh successful observation | plain content | danger styling; treating empty as error |
| **S-PART** | C1 `strip` above content; missing cells `—` | half-circle icon; «Часть данных недоступна: <что именно>»; caution | content controls unaffected | chip normal | `—` cells carry `aria-label` «нет данных» | rendering 0 for a missing value |
| **S-STALE** | content **retained** + C1 `strip`; C0 flips to `aged` | clock icon; «Данные устарели · возраст N мин»; caution | mutations acting on stale data add a warning line in their confirm dialog | age at the block + global header stale dot | plain; strip not dismissible while stale | replacing retained data; hiding age |
| **S-UNK** | «неизвестно» + icon in place of the value | question icon; neutral/grey | dependent mutations `disabled` **with visible reason text** + «диагностика» link | chip `unknown` | reason is real text, not tooltip-only | green tint; 0%; bare disabled controls |
| **S-DEGR** | C1 `strip`; content retained | triangle icon; «Работает частично: <причина>»; caution/warning | link to Диагностику/Логи **с префильтром подсистемы** | attempt/observation clocks per §12 where applicable | plain; not dismissible while active | masking as healthy; dropping retained content |
| **S-FAIL** | C1 `block` or `strip` at the failure site | octagon icon; «Сбой: <причина> · <время>»; danger | manual **Retry** (re-fires the same request); no invented auto-retry cadence | failure timestamp shown; data-freshness chip (if content retained) unchanged | **`role="alert"`**; inline, never toast; repeats update timestamp, no stacking | toasting errors; blanking retained data |
| **S-BLOCK** | C1 `block` | shield icon; «Заблокировано браузером» + enable steps; warning | test buttons remain gesture-gated | — | plain | auto-triggering the blocked capability |
| **S-DENY** | C1 `block` | slash icon; «Разрешение отклонено» + re-grant steps; danger | — | — | plain | conflating with S-BLOCK (they are 2 of the 4 explicit browser-permission states on `/events/browser`) |
| **S-NOBACK** | **nothing renders** — the DOM lacks the control/field | — | — | — | — | grey buttons, placeholders, tooltips, reserved gaps, any token |
| **S-SESS** | «⚑ сессия» C10 badge appended to chip/row/banner | flag icon; «свидетельство этой сессии»; caution | — | marks session-scoped evidence; survives filtering | tooltip + glossary link; badge text is real text | presenting session evidence as persistent history |
| **S-DEFER** | the single C18 card (queue page only) | pause icon; passive sentence; neutral | **none** | — | plain | grey controls, menu stubs, any second instance |

## 8. Responsive rules — complete contracts

Breakpoints (Tailwind defaults, no custom screens): `md` 768 · `lg` 1024 · `xl` 1280. Tablet/mobile are **target behavior** [AD], verified in S5-10. **Feature-parity rule: functions are never silently removed at any width.**

- **Desktop `xl` ≥1280** (design target 2560×1440): expanded 2-level sidebar 260px; 12-col grid, 24px gutters; slot pair side-by-side; dense tables full width; settings = category list (240px) + form pane; content max-width 1680px centered beyond.
- **Compact `lg` 1024–1279**: icon rail 56px with hover/focus flyouts (`--z-rail`); tables keep dense density but drop tertiary columns — each dropped column's **key status merges into the primary cell as a badge** (status is never dropped); KPI rows wrap 4→2.
- **Tablet `md` 768–1023**: drawer nav (scrim, focus trap, Escape); wide tables → C3 `list-card`s ordered **status first, then evidence, then metadata**; filter state persists across the transform; touch targets **≥44px**; slots still side-by-side.
- **Mobile <768**: drawer nav; slots stacked; settings open via a category index screen; claims/queue as cards; horizontal scroll only inside log/code wells; confirmations always full bottom-sheet modals (C8); compact log reader (C15); toolbar collapses to search + «Фильтры» disclosure.
- Slots: identical C12 geometry at all widths (pair ≥md, stack <md); empty slot stays rendered everywhere.
- Filters/forms: C5 collapses to disclosure on mobile, state preserved; C6 stays single-column stacked at all widths; C7 bar full-width sticky bottom on mobile.
- Logs/charts: log well keeps internal horizontal scroll; charts resize fluidly, summaries/tables always reachable.
- Page gutters 24/16/12px; page never scrolls horizontally; sticky limited to table header, save bar, page toolbar; drawer/dialog mechanics identical wherever present.

## 9. Keyboard, focus, contrast, reduced-motion, screen-reader contract

- **Landmarks**: `banner` (top bar) / `navigation` (sidebar + drawer) / `main` / `contentinfo`. Skip-link «К содержимому» = first focusable, visible on focus.
- **Headings**: exactly one `h1` per page; no level skips.
- **Keyboard**: DOM-order reachability (sidebar → toolbar → content); table sort headers are real buttons; save-bar buttons last in the category tab order; «К новым» chip focusable; rail flyouts open on focus (keyboard path).
- **Focus**: universal `:focus-visible` ring `outline: 2px solid var(--focus-ring); outline-offset: 2px`; **`outline: none` is banned** unless an equal-or-better visible replacement exists at the same selector; ring holds ≥3:1 against adjacent surfaces in both themes.
- **Dialogs/drawers**: focus trap; Escape closes/cancels; initial focus on least destructive action (dialogs); focus returns to invoker (both).
- **Tables**: `<th scope>`; `aria-sort`; polite sort-change announcement via the existing `data-*-announce` mechanic [CP]; missing values `—` with `aria-label`.
- **Charts**: paired text summary (ranges and gaps stated) + underlying table/CSV; per-series markers/dash patterns in addition to hue (P4).
- **Live regions**: exactly two globals — toast (`aria-live="polite"`) and lifecycle (`role="alert"`) [CP]; regional S-FAIL alerts are scoped to their region and removed on recovery; routine SSE/poll refreshes never announce.
- **Contrast (WCAG 2.1 AA)**: text ≥4.5:1 (≥3:1 at ≥24px or ≥18.66px bold); non-text UI ≥3:1. Verified pairs: `--text-primary/secondary` on all surfaces; tier inks on `--state-*-bg` (ink = full-strength tier over surface, re-validated per theme); `--log-*` on `--surface-inset`. Disabled text may fall below AA only with adjacent AA-compliant reason text (S-UNK rule). Known risk: bright `--ui-*` hues on light theme — ≥3:1 sizes/weights or inset surfaces only, tooling pass in S5-10.
- **Reduced motion**: global `@media (prefers-reduced-motion: reduce)` → all `--motion-*` to 0ms; static skeleton; chart animations off; drawer/dialog appear by opacity step only.
- **RU/EN**: all state copy and `aria-label`s through the i18n dictionary [CP]; Г2 (`events_drawer.html` EN strings) fixed in S5-7.


## 10. HTMX loading, swap, failure and focus behavior

Transport is fixed [CP] and **inventing transport is prohibited**: server-first render; SSE only for the miner-status stream (`/api/miner-status/stream`); 10s polling for logs; 30s polling for the sidebar `now-watching`. New zones (event journal, claims, queue history) use the **transport-independent contract**: C0 freshness chip + manual «Обновить» button — no new WebSocket/EventSource connections and no new poll cadences [AD, per Г5].

- **Stable `hx-target` boundaries** [CP pattern]: HTMX swaps only fragments with stable target containers — lifecycle panel, slots, queue/roster, logs, drops lists, journal, status widgets. Page shells render server-side; fragments swap inside fixed-geometry containers (P5).
- **Initial load vs refresh**: server-rendered S-LOAD skeletons only for genuinely late fragments (no skeleton theater for static content). A refresh of an already-populated region is **not** S-LOAD: `hx-indicator` shows a 60%-opacity skeleton overlay **over the retained old content**, which stays visible until the swap.
- **Mutations**: button spinner + `hx-disabled-elt` (`aria-busy`, 60% opacity, default cursor) + `hx-sync` to prevent double-fire [CP]. Light confirmations via `hx-confirm`; heavy ones via C8 modals.
- **Swap settle**: on **user-initiated, non-log** swaps the incoming fragment fades opacity 0.6→1 over `--motion-base` (`.htmx-settling`); no translate/slide. **Logs, SSE updates and timed-poll refreshes animate nothing** (§4). Under `prefers-reduced-motion` every swap is instant.
- **Success**: mutation success = C17 toast + fragment swap; the swapped region's C0 chip is the **only** freshness messaging (ties into §12: no swap → no visual event).
- **Inline errors, never toast**: HTMX response/network errors render the error partial **into the failing region** as C1 S-FAIL — `role="alert"`, cause, time, **Retry** re-firing the same request. Repeated failures update the strip's timestamp; strips never stack. Toasts carry success/neutral only.
- **Focus after swap**: container `tabindex="-1"` focus + `data-*-announce` [CP] for **user-initiated** swaps only; timed polls and SSE updates never steal focus and never announce.
- **Deep links**: filters/categories → query params via `replaceState`; back/forward restores filter UI; links always target the owner of the fact (Обзор→Очередь→Глоссарий; Дропсы↔Claims↔Журнал; События→Настройки; Настройки/system→Система; S-DEGR/S-FAIL→Логи with subsystem prefilter).

## 11. Page-application matrix — all 30 routes

Density: **D** dense / **S** standard / **R** reading. Transform codes: **T** = table⇄cards at `<lg` · **ST** = stack/reflow · **IDX** = mobile settings category index + stacked form · **LOG** = compact log reader · **R** = reading reflow. S-LOAD/S-READY apply everywhere; chrome C2/C9/C17 + §9 apply to all 30. "Owner" = what this page authoritatively owns (everything else it links to).

| # | Route (section) | Dens. | Components | Primary S-states | Gates | Transform | Authoritative owner |
|---|---|---|---|---|---|---|---|
| 1 | `/overview` (Обзор) | S | C3, C12 pair, C9, C10, C0 | S-UNK (lifecycle btns off + reason), S-DEGR, S-FAIL | Up Next S-NOBACK [BE:B7]; P3 version S-NOBACK [BE:B8]; alerts [BE:B1] | ST | lifecycle controls (Пауза/Перезапуск⚠/Стоп⚠⚠) [CP] |
| 2 | `/overview/queue` (Обзор) | D | C4, C5, C12 header, C18, C0 | S-EMPTY→настройки, S-PART `—`, S-SESS, S-DEFER | journal history S-SESS until [BE:B6]; sole DPBA surface | T | slot/queue/roster evidence; reason-codes display |
| 3 | `/drops/current` (Дропсы) | S | C13, C11, C5, C1 strip, C0 | R17 set (§12): S-DEGR/S-STALE retained, S-EMPTY, S-UNK | DP-C badges; account-link [BE:B11] | ST | policy selector + manual sync trigger [CP] |
| 4 | `/drops/upcoming` (Дропсы) | S | C13 catalog, C5 | S-EMPTY | no fabricated progress/claimability | ST | future-campaign catalog [CP] |
| 5 | `/drops/claims` (Дропсы) | D | C4, C5, C10 ×7, C11 timeline, C0 | S-SESS until [BE:B2]; S-UNK never «получен»/«сбой» | owner decision №1 open (period filter fits both) | T | **claim lifecycle — sole owner** |
| 6 | `/drops/past` (Дропсы) | D | C4, C5, C10 | S-SESS/S-UNK «исход неизвестен» | outcomes link into Claims | T | campaign history + ghost-skip [CP] |
| 7 | `/analytics/points` (Аналитика) | S | C14 + annotations, C5, export | S-EMPTY, S-PART | false «0» suppressed | ST | points chart/annotations/CSV [CP] |
| 8 | `/analytics/roi` (Аналитика) | D | C3 kpi, 3×C4, C5, Retry | S-EMPTY, S-FAIL | table composition [INT] (Г4); read-only | T | ROI KPIs/breakdowns/export [CP]; edits → route 16 |
| 9 | `/events` (События) | D | C4, C5, C10 | S-PART, S-SESS, S-UNK delivery | journal [BE:B1]; delivery column hidden without [BE:B3]; 5 event types only | T | product-event journal |
| 10 | `/events/browser` (События) | S | C1 4-state, gesture test | S-READY/S-BLOCK/S-DENY/S-UNK | test by gesture only | ST | browser-permission status |
| 11 | `/events/sound` (События) | S | C1 status, gesture test | S-BLOCK, S-UNK | **status only**; fail-open stated; prefs [BE:B4] | ST | sound **status** (config lives at route 20) |
| 12 | `/events/discord` (События) | S | C3, Тест [CP], C0 | S-UNK delivery [BE:B3], S-FAIL test | token never displayed | ST | Discord channel test + delivery status |
| 13 | `/settings/streamers` (Настройки) | S+D | C6, C4 roster, C7, C8 | C7 cycle, S-FAIL save | delete = heavy confirm | IDX+T | roster/import/overrides config |
| 14 | `/settings/rotation` (Настройки) | S | C6, C7, C9 jitter info | C7 cycle | jitter preserved, no disable control | IDX | rotation/priority/interval config |
| 15 | `/settings/drops` (Настройки) | S | C6, C7, link-card | C7 cycle | — | IDX | drops policy/discovery/sync config |
| 16 | `/settings/predictions` (Настройки) | S | C6, C7 | C7 cycle | — | IDX | **sole bet editor** |
| 17 | `/settings/chat-raids` (Настройки) | S | C6, C7 | C7 cycle | — | IDX | IRC/raids config |
| 18 | `/settings/transport` (Настройки) | S | C6, C7, C9 | C7 cycle | «влияет на здоровье» banner | IDX | limits/backoff config |
| 19 | `/settings/analytics-logging` (Настройки) | S | C6, C7 | C7 cycle | — | IDX | analytics/log-level config |
| 20 | `/settings/events-notifications` (Настройки) | S | C6 matrix, gesture preview, Сброс, C7 | C7 cycle, S-BLOCK preview | TZ [BE:B9]; Upload/Delete S-NOBACK [BE:B5]; prefs note [BE:B4]; fail-open line | IDX | sound/notification **config** — sole owner of `notification_config`'s user-configurable fields + `point_rules`' `streamer`/`threshold`/`delete_on_trigger` (never `id`/`triggered`) |
| 21 | `/settings/discord` (Настройки) | S | C6 masked token, C7 | C7 cycle | no «показать токен»; test at route 12; channels/rules editing → route 20 | IDX | Discord **connection** config only — `config.json` `enabled`+`GuildID` (read/write) + write-only `BotToken` |
| 22 | `/settings/system` (Настройки) | S | C6, read-only LAN, C7 | C7 cycle | config only; status → Система | IDX | canary/watchdog/updater **config** |
| 23 | `/system/status` (Система) | D | C4, **C0 per row**, links | S-UNK ≠ green, S-DEGR, S-STALE per row | freshness on every row mandatory | T | subsystem health snapshot display |
| 24 | `/system/diagnostics` (Система) | S | C16, C8, snapshot btn | S-FAIL canary, S-NOBACK version [BE:B8] | absence ≠ «последняя версия» | ST | canary run + **Diagnostic Snapshot (canonical)** [CP] |
| 25 | `/system/logs` (Система) | D | C15 (9 functions), C5 | S-FAIL stream, S-STALE poll age | logs ≠ события note | LOG | technical log viewer [CP] |
| 26 | `/help/getting-started` (Справка) | R | prose, link-cards | — | static [AD]; no live state | R | editorial content |
| 27 | `/help/glossary` (Справка) | R | mono definition list | — | codes from code dictionary [CP enums]; parity test S5-9 | R | code explanations (codes owned by code) |
| 28 | `/help/troubleshooting` (Справка) | R | prose + deep links | — | must distinguish неизвестно/устарело/деградация/сбой | R | editorial content |
| 29 | `/help/notifications-audio` (Справка) | R | prose + static themed SVG | — | fail-open model explained | R | editorial content |
| 30 | `/help/diagnostics-support` (Справка) | R | prose + link to route 24 | — | help never generates snapshots | R | editorial content |

**Route 20/21 ownership — owner-approved governance re-ownership before S5-6.** This narrows the earlier
"Discord token/channels/rules config" wording for route 21 and makes route 20's scope exhaustive over
user-configurable fields; it does not add, remove, or renumber any of the 30 routes and does not change any
B-gate's render behavior.

- **Rationale**: `NotificationConfig.SaveConfig` (`internal/notifications/repository.go`) full-row-writes every
  `notification_config` column on each save, so exactly one C7 category must own that row — otherwise two
  categories editing concurrently would stale-read and full-row-clobber each other.
- **Route 20** (`/settings/events-notifications`) is the **sole owner** of every user-configurable
  `notification_config` field (channel mappings, per-event enable toggles, streamer allow-lists,
  `upcoming_drops_enabled`) and of `point_rules`' UI CRUD/config fields — `streamer`, `threshold`,
  `delete_on_trigger`. It does **not** own `point_rules.id` (DB primary key) or `point_rules.triggered`
  (backend runtime state) — those stay backend-owned.
- **Route 21** (`/settings/discord`) owns only `config.json`'s Discord connection. `enabled` and `GuildID` are
  normal read/write settings, redisplayed on the page like any other field. `BotToken` alone stays
  write-only/masked/never read back — unchanged from its existing wording above. Channel and rule editing is
  route 20's domain; route 21 links out to route 20 for that.
- **Route 12** (`/events/discord`) is unchanged: it remains sole owner of Discord test/delivery-status, as
  already stated in its row above.
- **B4/B5/B9** (§13) remain **S-NOBACK** exactly as specified below — unchanged gating; this is a
  documentation correction of which route authors which stored field, not a gate change.

## 12. Mandatory R17 visual semantics (`/drops/current`) — exact

Backend guarantees [CP, merged PR #147]: full sync **aborts before publish** on failed/unusable Inventory — the last-known-good published pool, `Revision`, `BackendUpdatedAt`, `UpdateSource` are untouched, and the failed attempt is recorded via `SyncStatus.LastError`; stale lightweight sync results are revision-guard **discarded in full**; a valid empty Inventory is a normal successful sync. UI-visible evidence fields [CP, `handlers_drops.go`]: `Runs`, `LastSyncAt`, `LastSuccessAt` (zero-time serialized as 0 = unambiguous «never»), `LastError`, campaign counts. **No fabricated backend fields — render only this evidence.**

1. **Last-known-good retention**: on full-sync failure or unusable Inventory, campaign cards and all per-drop progress **stay rendered from the last-known-good pool**, with an **S-DEGR strip** above the list: «Последняя синхронизация не удалась · <LastSyncAt> · <LastError>» + Retry (manual sync affordance) + Логи link prefiltered to drops.
2. **Failed-attempt clock ≠ published-data freshness**: the strip carries the *attempt* clock (`LastSyncAt` + error); the C0 chip on the campaign list carries the *data* clock (`LastSuccessAt` / published-pool freshness + source «полная синхр.»). They are distinct components in distinct positions and are never merged; the strip may be danger-adjacent while the chip is merely `aged`.
3. **S-STALE/S-DEGR treatment**: data aging without a new success escalates the chip to S-STALE («возраст N мин»), content retained. S-STALE and S-DEGR with age/reason are the **only** states permitted for retained data; retained progress is **never** replaced with 0% or «неизвестно» because an attempt failed.
4. **Discarded stale lightweight result → no visual mutation**: no progress change, no chip/freshness change, no source change, no success messaging, no swap. At most a backend DEBUG log — the UI renders nothing for it.
5. **Valid empty Inventory = S-EMPTY**, never S-FAIL: «Нет активных кампаний по вашей политике» + links to `/settings/drops` and `/drops/upcoming`; the chip shows a *fresh, successful* sync.
6. A drop with no authoritative observation ever renders C11 «прогресс неизвестен» (S-UNK) — visually distinct from both 0% and stale-retained values.

## 13. B1–B11 visual gates, DP-C and DPBA — complete registers

Global rule: **no B-dependency field is ever rendered as available, and absence never produces grey placeholders** — either the evidence-gated element is absent (S-NOBACK), or the page runs visibly degraded (S-SESS / «неизвестно»).

| # | Dependency | Routes / components | Blocking? | Render behavior until evidence | Evidence required to enable |
|---|---|---|---|---|---|
| B1 | Storage for the 5-product-event journal | `/events` (9), nav badges (C2) | **blocks** full journal | /events shows session-scope banner; nav badges **absent** (S-NOBACK), never «0» | event storage/retrieval contract |
| B2 | Claims persistence: 7 states + timeline | `/drops/claims` (5), C10/C11 | **blocks** full history | page runs degraded: «⚑ сессия» banner + S-SESS rows | claims persistence contract |
| B3 | Delivery/playback status capture | `/events` (9), `/events/discord` (12) | no | delivery columns/facets **hidden**; last-delivery shows «неизвестно» | delivery-result recording |
| B4 | Account-level sound preferences | routes 20, 11 | **blocks** sound persistence | prefs-persistence note gated; sound status page renders regardless | preferences store |
| B5 | Custom sound file storage | Upload/Delete in route 20 | no | controls **not rendered** (S-NOBACK) | sound file storage |
| B6 | Public contract for reason-codes/eligibility/slot-journal | `/overview/queue` (2) | partial | journal history marked S-SESS; internal CP structures may render | UI delivery contract |
| B7 | Up Next candidate | routes 1, 2 | no | widget **not rendered** (S-NOBACK) | next-candidate field |
| B8 | SHA/digest/build time | route 24, Обзор P3 | no | conditional render; absence = «данные о сборке недоступны», never «последняя версия» | fields in status snapshot |
| B9 | Quiet-hours timezone source | route 20 | **blocks** TZ field | field **not rendered** | TZ source |
| B10 | restart-required signal | C7 banners, Обзор | no | banner only on actual signal; never fabricated | backend signal |
| B11 | Account↔campaign link | C13 on route 3 | no | link badge **absent** without evidence | eligibility-status delivery |

**DP-C** (DESIGN_PROPOSAL_PENDING_FILLED_GROUP_C_EVIDENCE): filled campaign cards on `/drops/current` carry the `type-micro` outline badge «DP-C: макет по свидетельствам группы C» — visibly provisional; never presented as parity; removed only when Group C evidence upgrades the card, by a separate task.

**DPBA** [DEF] — all five deferred manual controls: **(1)** звезда/избранное назначения · **(2)** play/принудительное переключение слота · **(3)** Harvest · **(4)** ACL-подобное переопределение · **(5)** ручная перестановка очереди (drag-and-drop not designed). They exist in the UI **only** as the single passive C18 card on `/overview/queue` — no buttons, no menu items, no greyed ghosts, no hover hints, anywhere. Lifting the deferral requires a separate task carrying broker-audit results.

## 14. Representative high-fidelity mockups (from the completed report)

Desktop `xl`, dark theme, RU copy. `⟦…⟧` = component/token annotation. Chrome shown once, then elided.

### 14.1 `/overview`

```text
┌ app ───────────────────────────────────────────────────────────────────────┐
│ ⟦C2 sidebar 260px, surface-sidebar⟧      ⟦topbar: theme ◐ | RU EN | ●stale⟧ │
│ ▍Обзор            │  Обзор                                    ⟦type-h1⟧    │
│   Обзор           │  ⟦C9: скрытые #health-banner/#lifecycle-auth-banner⟧   │
│   Очередь         │ ┌ Майнер: Работает ⟦ui-online●+текст⟧  ▲ 8 с · SSE ⟦C0⟧┐│
│ ▸ Дропсы          │ │ [ Пауза ]  Дополнительно ▾ (Перезапуск⚠ · Стоп⚠⚠)   ││
│ ▸ Аналитика       │ │ ⟦S-UNK: кнопки disabled + «состояние неизвестно —   ││
│ ▸ События и увед. │ │  переход к диагностике», текст рядом, не tooltip⟧   ││
│ ▸ Настройки       │ └─────────────────────────────────────────────────────┘│
│ ▸ Система         │ ┌ Слот 1 ⟦C12, 2px brand⟧ ┐ ┌ Слот 2 ⟦C12 dashed⟧ ────┐│
│ ▸ Справка         │ │ ◉ streamer_a  ●В эфире  │ │ Слот свободен           ││
│ ──────────────    │ │ REASON: DROP_PRIORITY   │ │ причина: NO_CANDIDATE   ││
│ ⟦now-watching     │ │ 2ч 14м · +1 250 ⟦ui-gain⟧│ │ → Очередь              ││
│  мини-слоты CP⟧   │ └─────────────────────────┘ └─────────────────────────┘│
│                   │ ┌ Здоровье ⟦C3⟧ ─────────┐ ┌ Прогнозы ⟦C3 kpi⟧ ──────┐│
│                   │ │ ✓OAuth ✓GQL ✓PubSub    │ │ Сегодня: +340 · ROI +4%  ││
│                   │ │ ⚠Синхр. дропсов        │ │ ⟦ui-roi-pos, tabular⟧    ││
│                   │ │ «работает частично» →  │ └──────────────────────────┘│
│                   │ │ Диагностика ⟦S-DEGR⟧   │ ⟦Up Next: НЕ РЕНДЕРИТСЯ —  │
│                   │ └────────────────────────┘  S-NOBACK (B7); версия P3   │
│                   │                              тоже S-NOBACK (B8)⟧       │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 14.2 `/overview/queue`

```text
│ Очередь и назначения                                  ⟦type-h1⟧
│ ┌ Слот 1 ◉ streamer_a · DROP_PRIORITY ┐┌ Слот 2 — свободен · NO_CANDIDATE ┐ ⟦C12⟧
│ ⟦C5⟧ [Поиск… ] (Статус ▾)(Причина ▾)(Сортировка ▾)  [таблица|карточки]  Сбросить
│ ┌ Полный ростер ─────────────────────────── ⏱ 12 с назад · опрос ⟦C0⟧ ┐
│ │ Стример▲     Статус      Причина⟦mono⟧      Очередь   Баллы    Дропсы │ ⟦C4 32px⟧
│ │ streamer_a   ●В эфире    DROP_PRIORITY→ⓘ    #1        128 400   2/3   │
│ │ streamer_b   ●В эфире    ELIGIBLE→ⓘ         #2        67 210    —     │ ⟦S-PART:
│ │ streamer_c   ○Офлайн     OFFLINE→ⓘ          —         12 050    —     │  «—» не 0⟧
│ │ ⟦история слот-журнала: столбец с бейджем «⚑ сессия» ⟦S-SESS до B6⟧⟧    │
│ └───────────────────────────────────────────────────────────────────────┘
│ ┌ ⟦C18 · S-DEFER, нейтральный, без кнопок⟧ ──────────────────────────────┐
│ │ ⏸ Ручное управление (избранное, переключение слота, harvest,           │
│ │   переопределения, перестановка очереди) отложено до аудита брокера.   │
│ └────────────────────────────────────────────────────────────────────────┘
│ ⟦S-EMPTY ростера: C1 block «Ростер пуст — добавьте стримеров» → /settings/streamers⟧
```

### 14.3 `/drops/current` — R17 state shown

```text
│ Дропсы · Текущие                                     ⟦type-h1⟧
│ Политика: (Все ▾)  [Синхронизировать ⟳]              ⟦C5 + manual sync⟧
│ ┌ ⟦C1 strip · S-DEGR, caution rail 3px⟧ ────────────────────────────────┐
│ │ ⚠ Последняя синхронизация не удалась · 14:32 · inventory: HTTP 500    │
│ │   [Повторить]        Логи (дропсы) →        ⟦попытка ≠ свежесть данных⟧│
│ └───────────────────────────────────────────────────────────────────────┘
│ ┌ Кампания «Rust Twitch Drops №12» ⟦C13 + бейдж DP-C⟧ ──────────────────┐
│ │ [art] Rust · политика: allow ⟦C10⟧   ⟦B11 бейдж связи: НЕ рендерится⟧ │
│ │  Drop 1  ████████░░ 80% · 96/120 мин ⟦C11, mono, last-known-good⟧     │
│ │  Drop 2  «прогресс неизвестен» ⟦S-UNK: без бара, не 0%⟧               │
│ │  Забрать награды → Дропсы/Клеймы ⟦authority-ссылка⟧                   │
│ │            ⟦C0 aged/S-STALE⟧ ⟳ полная синхр. · успех 13:58 · 34 мин   │
│ └───────────────────────────────────────────────────────────────────────┘
│ ⟦Инварианты R17: карточки/прогресс СОХРАНЕНЫ при сбое; строка сбоя (14:32)
│  и свежесть данных (13:58) — разные компоненты; отброшенный устаревший
│  лёгкий синк не меняет НИЧЕГО; валидный пустой Inventory → S-EMPTY:
│  «Нет активных кампаний по вашей политике» + /settings/drops, /drops/upcoming⟧
```

### 14.4 `/drops/claims`

```text
│ Дропсы · Клеймы                                      ⟦type-h1⟧
│ ┌ ⟦C9 info⟧ История в объёме этой сессии ⚑ — постоянное хранилище не
│ │ подтверждено (до свидетельств B2)                                     ┘
│ ⟦C5⟧ (Кампания ▾)(Состояние ▾)(Период ▾)                       Сбросить
│ ┌ Таблица ─────────────────────────────── ⏱ 40 с назад · опрос ⟦C0⟧ ┐
│ │ Время⟦mono⟧   Кампания      Drop        Состояние⟦C10 7 клейм-сост.⟧│
│ │ 14:21:07 ⚑    Rust №12      Drop 1      ✓ Получен ⟦state-ok⟧        │
│ │ 13:02:44 ⚑    Rust №12      Drop 3      ↻ В обработке ⟦caution⟧     │
│ │ 12:58:00 ⚑    STALKER 2     Skin        ? Неизвестно ⟦S-UNK: никогда │
│ │                                           не «получен»/«сбой»⟧      │
│ └─────────────────────────────────────────────────────────────────────┘
│ ┌ Таймлайн выбранного клейма ⟦C11 timeline, mono-штампы, tier-точки⟧ ──┐
│ │ 14:20:59 ● замечен клеймабельным   14:21:03 ● запрошен               │
│ │ 14:21:07 ● подтверждён ledger ⟦CP-свидетельство⟧                     │
│ └──────────────────────────────────────────────────────────────────────┘
```

### 14.5 `/settings/events-notifications`

```text
│ Настройки · События и уведомления       ⟦категория = граница dirty⟧
│ ┌ Звук ⟦C6⟧ ────────────────────────────────────────────────────────────┐
│ │ Громкость      [────●────] 70%        [▶ Предпросмотр] ⟦по жесту⟧     │
│ │ ⟦S-BLOCK предпросмотра: C1 «Заблокировано браузером» + как включить⟧  │
│ │ Тихие часы     [22:00]–[08:00]  ⟦поле пояса: НЕ рендерится до B9⟧     │
│ │ ⓘ Сбой воспроизведения не останавливает майнер (fail-open).           │
│ │ ⟦Upload/Delete своих звуков: НЕ рендерятся — S-NOBACK (B5)⟧           │
│ ├ По событиям ──────────────────────────────────────────────────────────┤
│ │ Событие              Звук   Браузер   Discord      ⟦матрица toggles,  │
│ │ Клейм дропа          [вкл]  [вкл]     [выкл]        текст у каждого⟧  │
│ │ …ровно 5 продуктовых событий…                                         │
│ │ Профили: (Тихий ▾)   [Сброс к умолчанию]                              │
│ └───────────────────────────────────────────────────────────────────────┘
│ ┄┄ грязное состояние ┄┄
│ ┌ ⟦C7 sticky, z-10, shadow-1⟧ Изменений: 3 · категория «События и увед.»┐
│ │                                    [Отменить]  [Сохранить] ⟦primary⟧  │
│ └──────────────────────────────────────────────────────────────────────┘
│ ⟦уход со страницы → C8 Discard-диалог: Сохранить/Отменить изменения/Остаться⟧
```

### 14.6 `/system/status`

```text
│ Система · Статус                                      ⟦type-h1⟧
│ ┌ Подсистемы ⟦C4, свежесть НА КАЖДОЙ строке⟧ ───────────────────────────┐
│ │ Подсистема        Состояние⟦икона+текст+цвет⟧      Свежесть ⟦C0/строку⟧│
│ │ Жизненный цикл    ✓ Работает ⟦state-ok⟧            ▲ 5 с · SSE        │
│ │ OAuth             ✓ Токен активен                  ⏱ 2 мин · опрос    │
│ │ GraphQL           ✓ Норма                          ⏱ 2 мин            │
│ │ PubSub            ⚠ Переподключение ⟦S-DEGR⟧       ⏱ 40 с             │
│ │ Синхр. инвентаря  ⚠ Устарело · 34 мин ⟦S-STALE⟧    ⟳ 13:58            │
│ │ Ресурсы хоста     CPU 12% ⟦rw-cpu⟧ · RAM 48% ⟦rw-mem⟧  ⏱ 10 с         │
│ │ Хранилище         ? Неизвестно ⟦S-UNK: НЕ зелёный, серый + «?»⟧       │
│ └───────────────────────────────────────────────────────────────────────┘
│ [→ Диагностика]   [→ Логи]           ⟦S-DEGR строки линкуют с префильтром⟧
```

### 14.7 `/system/logs`

```text
│ Система · Логи        ⓘ Логи — технический журнал; события → /events
│ ⟦C5⟧ [Поиск… ] (Уровень ▾)(Модуль ▾)  [Перенос строк] [Копировать]
│ ┌ ⟦C15 well: surface-inset, type-code 12.5px⟧ ── ⏱ 10 с · опрос ⟦C0⟧ ──┐
│ │ 14:32:01 WRN ⟦log-warning⟧ drops    inventory acquisition failed …    │
│ │ 14:32:04 INF ⟦log-info⟧    watcher  minute watched streamer_a         │
│ │ 14:32:09 DBG ⟦log-debug⟧   pubsub   ping (+2.1s jitter)               │
│ │ ⟦подсветка поиска: brand 25% fill; уровень окрашен ТОЛЬКО в токене     │
│ │  уровня, сообщение — text-primary⟧                                    │
│ │            ⟦скролл приостановлен⟧ → [▼ К новым] ⟦floating chip⟧       │
│ └───────────────────────────────────────────────────────────────────────┘
│ «Показаны последние 2 000 строк» ⟦предел строк — явная надпись⟧
│ ⟦S-FAIL потока: C1 strip «Соединение потеряно · 14:33 · Повторить» role=alert⟧
```

## 15. Migration seams and compatibility strategy

Two deep modules, then route-by-route migration; the dashboard is fully functional at every commit.

- **Seam 1 — token layer** (`input.css`): extend additively with Layer D/E, `type-*`/`motion-*` classes; zero rendered-pixel change to existing pages. Legacy aliased names stay valid until S5-10 (grep-verified zero references before deletion).
- **Seam 2 — component partial library** (`templates/components/`, C0–C18): small template interfaces (§6 inputs); all 13-state discipline lives behind them; existing `partials/` untouched; a page adopts a component by swapping one `{{template}}` call.
- **Route mapping** (existing → new): `overview.html` (+`lifecycle_panel`, `now_watching`, `overview_live`) → routes 1–2 · `drops.html` (+`drops_list/past/upcoming`, `discovery_list`) → routes 3–6 (claims is the only net-new data page, starts S-SESS) · `statistics.html` → routes 7–8 (split along the existing `handlers_statistics.go`/`handlers_roi.go` seam) · `notifications.html`+`events_drawer.html` → routes 9–12 + 20–21 (enforces status/config split; fixes Г2) · `health.html` → routes 22–24 (resolves Г3, both parities preserved) · `logs.html` → route 25 (all 9 CP functions) · `settings.html`+`streamer.html` → routes 13–22 · `dashboard.html` retired last with server-side redirects.
- **Guard rails per slice**: `go test -race ./...` green; old/new routes may coexist with redirects; **no viewmodel/handler contract changes** — where data is absent, the answer is S-NOBACK/S-SESS, never a new field; nav flips to the 7-section tree only when each section has its landing route (end of S5-2).

## 16. Stage 5 slices S5-1…S5-10

| Slice | Content | Depends on | Risk note |
|---|---|---|---|
| S5-1 | Layer D/E tokens, `type-*`/`motion-*`, focus-ring audit, reduced-motion globals | — | near-zero visual delta; unlocks all |
| S5-2 | C0, C1, C10, C11, C17 + chrome (7-section sidebar/rail/drawer, redirects) | S5-1 | highest-visibility diff; do while pages familiar |
| S5-3 | `/overview` + `/overview/queue` (C12, C4⇄C3, C18) | S5-2 | lifecycle controls = highest blast radius; S-UNK gating tested first |
| S5-4 | Drops group: routes 3–6 (**R17 visuals**, DP-C, S-SESS claims) | S5-2 | verify against PR #147 test-harness states; claims net-new |
| S5-5 | System group: routes 23–25; Г3 split | S5-2 | parity-critical (9 log functions, snapshot owner) |
| S5-6 | C6/C7/C8 canon + 10 settings categories (B5/B9 gates; `/settings/system` re-home) | S5-2 | dirty-state canon trickiest; pilot one category (rotation), then stamp out |
| S5-7 | Events group: routes 9–12 (4-state permission blocks, gesture tests, Г2 fix) | S5-6 | status↔config split needs routes 20–21 live |
| S5-8 | Analytics: routes 7–8 (C14 theming, summaries, export) | S5-1 | isolated, low coupling |
| S5-9 | Help section: routes 26–30 (glossary wired to code dictionary + parity test) | S5-3–7 | glossary must reference final rendered codes |
| S5-10 | Retirement & audit: delete legacy aliases + `dashboard.html`; WCAG AA tooling pass both themes; keyboard walk of all 30 routes; RU/EN copy review | all | the only slice allowed to delete |

One PR-sized change per slice, each under its own task contract; per-slice review checklist = §18 scoped to touched routes.

## 17. Verification debts and open owner decisions

- **Debt 1**: tablet/mobile responsive behavior is *target*, not confirmed parity — verify via `webapp-testing` in S5-10 (and per-slice for transformed tables).
- **Debt 2**: bright `--ui-*` hues on the light theme need the S5-10 AA contrast tooling pass; until then restrict to ≥3:1 sizes/weights or inset surfaces.
- **Owner decision №1 (open, do not resolve in Stage 5)**: claims history depth — (a) bounded window (report's example: 90 days) with period picker vs (b) unlimited. Belongs to the B2 backend task; the UI (period filter) supports both.
- **Gaps carried**: Г1 stage docs unpersisted (recommend a separate write-task to commit Stage 1–4 reports before S5-1); Г2 fixed in S5-7; Г3 resolved by S5-5; Г4 ROI table composition is [INT] — mark in template comment; Г5 new-zone transport unknown — transport-independent contract stands.

## 18. Final acceptance checklist (per slice and at S5-10)

1. 7 sections / 30 routes intact — none added, removed, or renamed; no re-ownership beyond the sole
   owner-approved routes-20/21 field-ownership correction in §11/§13 — no further re-ownership relative to the
   canonical §11 mapping.
2. All 13 S-states render per §7; unknown never converts to positive; S-NOBACK = absence, never grey.
3. R17: last-known-good retained; attempt clock ≠ data clock (distinct components); discarded stale sync renders nothing; valid empty Inventory = S-EMPTY; only existing evidence fields.
4. Stage 2 canon: exactly two slots (empty = definite state); full roster; Claims sole authority; C7 canon on all 10 categories; no autosave; fail-open stated; DPBA = single passive C18 card only.
5. No B1–B11 field rendered as available; DP-C badges present; no fabricated data.
6. No React/Next/SPA; no CDN/external fonts (vendored Inter/JetBrains Mono only); no backend/API/schema changes; no new transport.
7. Templates use semantic tokens only; both palettes define every token; primitives absent from templates.
8. §9 a11y contract: landmarks, skip-link, one h1, focus ring (no bare `outline:none`), traps/Escape/return, `<th scope>`+`aria-sort`, chart summaries, reduced motion, exactly two global live regions, AA contrast.
9. HTMX: retained-content refresh (not S-LOAD), inline S-FAIL with Retry (never toast), errors never as success, focus only on user-initiated swaps.
10. Jitter paths untouched; token never displayed; missing version field never reads «последняя версия».
11. `go test -race ./...` green; `make lint` clean; existing page parity tests pass.
12. RU/EN localization complete for all new copy (incl. `aria-label`s).

---

Extraction complete — sourced solely from the completed Stage 4 report in this session; no research, no repository inspection or modification, no commits/branches/push/PR/Issue/comments/workflow actions. Final mode **READ_ONLY**. STOP.


STAGE4_IMPLEMENTATION_HANDOFF_COMPLETE
