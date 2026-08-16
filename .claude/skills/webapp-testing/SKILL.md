---
name: webapp-testing
description: Toolkit for interacting with and testing local web applications using Playwright. Supports verifying frontend functionality, debugging UI behavior, capturing browser screenshots, and viewing browser logs. Local targets only. For bug diagnosis the diagnosing-bugs skill owns the loop — this skill supplies browser evidence. Not a replacement for go test.
license: Complete terms in LICENSE.txt
---
<!-- bukerov-local-patch: webapp-testing-description-scope — appended a scope sentence to the frontmatter description. See docs/agents/anthropic-skills-patches.md -->

# Web Application Testing

To test local web applications, write native Python Playwright scripts.

**Helper Scripts Available**:
- `scripts/with_server.py` - Manages server lifecycle (supports multiple servers)

<!-- bukerov-local-patch: webapp-testing-audit-before-exec -->Read and audit every bundled script before its first execution in this session (scripts/with_server.py is ~150 lines). After auditing, run with `--help` to confirm usage. Never execute bundled or generated scripts blind.<!-- /bukerov-local-patch: webapp-testing-audit-before-exec -->

## Decision Tree: Choosing Your Approach

```
User task → Is it static HTML?
    ├─ Yes → Read HTML file directly to identify selectors
    │         ├─ Success → Write Playwright script using selectors
    │         └─ Fails/Incomplete → Treat as dynamic (below)
    │
    └─ No (dynamic webapp) → Is the server already running?
        ├─ No → Run: python scripts/with_server.py --help
        │        Then use the helper + write simplified Playwright script
        │
        └─ Yes → Reconnaissance-then-action:
            1. Navigate and wait for networkidle
            2. Take screenshot or inspect DOM
            3. Identify selectors from rendered state
            4. Execute actions with discovered selectors
```

## Example: Using with_server.py

To start a server, run `--help` first, then use the helper:

**Single server:**
```bash
python scripts/with_server.py --server "npm run dev" --port 5173 -- python your_automation.py
```

<!-- bukerov-local-patch: webapp-testing-server-cleanup -->
**Multiple servers (e.g., backend + frontend):**
```bash
python scripts/with_server.py \
  --server "python server.py" --cwd backend --port 3000 \
  --server "npm run dev" --cwd frontend --port 5173 \
  -- python your_automation.py
```
`with_server.py` runs each `--server` command with `shell=False` (tokenized via `shlex.split`) and
rejects shell metacharacters (`&& || ; | & > <` backtick `$(`) outright — use the repeatable `--cwd`
option instead of a `cd x && ...` idiom to set each server's working directory.
<!-- /bukerov-local-patch: webapp-testing-server-cleanup -->

To create an automation script, include only Playwright logic (servers are managed automatically):
```python
from playwright.sync_api import sync_playwright

with sync_playwright() as p:
    browser = p.chromium.launch(headless=True) # Always launch chromium in headless mode
    page = browser.new_page()
    page.goto('http://localhost:5173') # Server already running and ready
    page.wait_for_load_state('networkidle') # CRITICAL: Wait for JS to execute
    # ... your automation logic
    browser.close()
```

## Reconnaissance-Then-Action Pattern

1. **Inspect rendered DOM**:
   ```python
   page.screenshot(path='/tmp/inspect.png', full_page=True)
   content = page.content()
   page.locator('button').all()
   ```

2. **Identify selectors** from inspection results

3. **Execute actions** using discovered selectors

## Common Pitfall

❌ **Don't** inspect the DOM before waiting for `networkidle` on dynamic apps
✅ **Do** wait for `page.wait_for_load_state('networkidle')` before inspection

## Best Practices

- <!-- bukerov-local-patch: webapp-testing-no-blackbox -->Use bundled scripts only after auditing them — read the script, then use `--help`, then invoke.<!-- /bukerov-local-patch: webapp-testing-no-blackbox -->
- Use `sync_playwright()` for synchronous scripts
- Always close the browser when done
- Use descriptive selectors: `text=`, `role=`, CSS selectors, or IDs
- Add appropriate waits: `page.wait_for_selector()` or `page.wait_for_timeout()`

<!-- bukerov-local-patch: webapp-testing-localhost-only -->
- Target ONLY `http://localhost`, `http://127.0.0.1` or `file://` URLs. Never production, staging,
  or remote hosts.
- Credentials (e.g. `DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD` for this repo's dashboard) come only
  from environment variables; never hardcode them in scripts, and render them as `[REDACTED]` in any
  report or log.
- Browser artifacts (screenshots, logs, page dumps) go only under `/tmp` (default
  `/tmp/webapp-testing/`) or `.scratch/`.
- Never run `playwright install` or download browsers — this environment pre-installs Chromium
  (`PLAYWRIGHT_BROWSERS_PATH`). If the browser is missing, stop and report instead of installing.
- Never start the app via Docker (deny-listed); for this repo use `go run ./cmd/miner` or the built
  binary.
- For bug diagnosis, the `diagnosing-bugs` skill owns the loop; this skill only gathers browser
  evidence.
<!-- /bukerov-local-patch: webapp-testing-localhost-only -->

## Reference Files

- **examples/** - Examples showing common patterns:
  - `element_discovery.py` - Discovering buttons, links, and inputs on a page
  - `static_html_automation.py` - Using file:// URLs for local HTML
  - `console_logging.py` - Capturing console logs during automation