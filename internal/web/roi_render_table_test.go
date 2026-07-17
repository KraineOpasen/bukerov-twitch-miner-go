package web

import (
	"os"
	"strings"
	"testing"
)

// renderTableBody extracts just the body of the ROI `renderTable(tableId, rows)`
// function from the statistics template, by brace-matching from its opening
// brace to the matching close. The assertions below inspect only this fragment,
// never the whole template, so an unrelated change elsewhere can't affect them.
func renderTableBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("templates/statistics.html")
	if err != nil {
		t.Fatalf("read statistics template: %v", err)
	}
	const marker = "function renderTable(tableId, rows) {"
	start := strings.Index(string(src), marker)
	if start < 0 {
		t.Fatalf("renderTable(tableId, rows) not found in statistics.html")
	}
	depth := 0
	for i := start + len(marker) - 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(src[start : i+1])
			}
		}
	}
	t.Fatal("renderTable body not terminated")
	return ""
}

// TestROIRenderTableNoI18nShadowing is a regression guard for the Prediction ROI
// bug "TypeError: t is not a function": renderTable used a local `const t =
// document.getElementById(tableId)` that shadowed the global i18n t() the header
// cells call, so any non-empty ROI table threw. It inspects only the renderTable
// body (not a global search over the template).
func TestROIRenderTableNoI18nShadowing(t *testing.T) {
	body := renderTableBody(t)

	// 1. No local declaration named t — that is exactly the shadow that broke it.
	for _, decl := range []string{"const t =", "const t=", "let t =", "let t=", "var t =", "var t="} {
		if strings.Contains(body, decl) {
			t.Errorf("renderTable declares %q, shadowing the global i18n t():\n%s", decl, body)
		}
	}

	// 2. The DOM element is named with a clear, non-shadowing identifier.
	if !strings.Contains(body, "const table =") && !strings.Contains(body, "const tableEl =") {
		t.Errorf("renderTable should name its DOM element `table`/`tableEl`:\n%s", body)
	}

	// 3. The i18n header calls remain (and must resolve to the global function).
	if !strings.Contains(body, "t('js.stat.col.key')") || !strings.Contains(body, "t('js.stat.col.profit')") {
		t.Errorf("renderTable lost its i18n header calls:\n%s", body)
	}

	// 4. innerHTML is written through the DOM variable, never through `t`.
	if !strings.Contains(body, "table.innerHTML") && !strings.Contains(body, "tableEl.innerHTML") {
		t.Errorf("renderTable should assign innerHTML via the DOM variable:\n%s", body)
	}
	if strings.Contains(body, "t.innerHTML") {
		t.Errorf("renderTable still writes via `t.innerHTML` (the shadow):\n%s", body)
	}

	// 5. The empty-state branch is preserved (renders the placeholder row).
	if !strings.Contains(body, "rows.length === 0") || !strings.Contains(body, "—</td>") {
		t.Errorf("renderTable lost its empty-table handling:\n%s", body)
	}
}
