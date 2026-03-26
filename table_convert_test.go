package main

import (
	"strings"
	"testing"
)

func TestConvertMarkdownTables_SimpleTable(t *testing.T) {
	input := "| Name | Age |\n|------|-----|\n| Alice | 30 |"
	result, placeholders := convertMarkdownTables(input)

	// Result should have exactly one placeholder
	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}

	// Get the rendered table
	var rendered string
	for _, v := range placeholders {
		rendered = v
	}

	t.Logf("Rendered: %s", rendered)

	if !strings.HasPrefix(rendered, "<pre>") || !strings.HasSuffix(rendered, "</pre>") {
		t.Errorf("expected <pre> wrapper, got: %s", rendered)
	}
	if strings.Contains(rendered, "---") {
		t.Errorf("separator row should be removed, got: %s", rendered)
	}
	if !strings.Contains(rendered, "Alice") {
		t.Errorf("missing data cell 'Alice': %s", rendered)
	}
	if !strings.Contains(rendered, "Name") {
		t.Errorf("missing header 'Name': %s", rendered)
	}

	// Result line should be the placeholder
	for k := range placeholders {
		if !strings.Contains(result, k) {
			t.Errorf("result should contain placeholder %s", k)
		}
	}
}

func TestConvertMarkdownTables_MixedContent(t *testing.T) {
	input := "Hello\n| a | b |\n|---|---|\n| 1 | 2 |\nGoodbye"
	result, placeholders := convertMarkdownTables(input)

	t.Logf("Result: %s", result)

	if !strings.Contains(result, "Hello") {
		t.Error("missing 'Hello' in result")
	}
	if !strings.Contains(result, "Goodbye") {
		t.Error("missing 'Goodbye' in result")
	}
	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}
	for _, v := range placeholders {
		if !strings.Contains(v, "<pre>") {
			t.Error("table placeholder should contain <pre>")
		}
	}
}

func TestConvertMarkdownTables_SpecialCharacters(t *testing.T) {
	input := "| x < y | a & b |\n|--------|-------|\n| c > d | ok |"
	_, placeholders := convertMarkdownTables(input)

	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}

	var rendered string
	for _, v := range placeholders {
		rendered = v
	}

	t.Logf("Rendered: %s", rendered)

	if !strings.Contains(rendered, "&lt;") {
		t.Errorf("< should be escaped to &lt;: %s", rendered)
	}
	if !strings.Contains(rendered, "&amp;") {
		t.Errorf("& should be escaped to &amp;: %s", rendered)
	}
	if !strings.Contains(rendered, "&gt;") {
		t.Errorf("> should be escaped to &gt;: %s", rendered)
	}
}

func TestConvertMarkdownTables_WideTable(t *testing.T) {
	input := "| A | B | C | D | E | F |\n|---|---|---|---|---|---|\n| 1 | 2 | 3 | 4 | 5 | 6 |"
	_, placeholders := convertMarkdownTables(input)

	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}

	var rendered string
	for _, v := range placeholders {
		rendered = v
	}

	t.Logf("Rendered: %s", rendered)

	// Should have all 6 columns
	if !strings.Contains(rendered, "A") || !strings.Contains(rendered, "F") {
		t.Error("missing columns in wide table")
	}
}

func TestConvertMarkdownTables_EmptyCells(t *testing.T) {
	input := "| a |  | c |\n|---|---|---|\n| 1 | 2 | 3 |"
	_, placeholders := convertMarkdownTables(input)

	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}

	var rendered string
	for _, v := range placeholders {
		rendered = v
	}

	t.Logf("Rendered: %s", rendered)

	if !strings.Contains(rendered, "<pre>") {
		t.Error("should produce <pre> block")
	}
}

func TestConvertMarkdownTables_AlignmentMarkers(t *testing.T) {
	input := "| h1 | h2 |\n|:---|---:|\n| left | right |"
	_, placeholders := convertMarkdownTables(input)

	if len(placeholders) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(placeholders))
	}

	var rendered string
	for _, v := range placeholders {
		rendered = v
	}

	t.Logf("Rendered: %s", rendered)

	// Separator with alignment markers should be removed
	if strings.Contains(rendered, ":---") || strings.Contains(rendered, "---:") {
		t.Errorf("alignment markers should be removed: %s", rendered)
	}
}

func TestConvertMarkdownTables_NoTable(t *testing.T) {
	input := "Just plain text here\nNo tables at all"
	result, placeholders := convertMarkdownTables(input)

	if len(placeholders) != 0 {
		t.Errorf("expected 0 placeholders for plain text, got %d", len(placeholders))
	}
	if result != input {
		t.Errorf("plain text should pass through unchanged\ngot:  %s\nwant: %s", result, input)
	}
}

func TestConvertMarkdownTables_MultipleTables(t *testing.T) {
	input := "Table 1:\n| a | b |\n|---|---|\n| 1 | 2 |\nMiddle text\n| c | d |\n|---|---|\n| 3 | 4 |\nEnd"
	result, placeholders := convertMarkdownTables(input)

	t.Logf("Result: %s", result)

	if len(placeholders) != 2 {
		t.Fatalf("expected 2 placeholders, got %d", len(placeholders))
	}
	if !strings.Contains(result, "Table 1:") {
		t.Error("missing 'Table 1:'")
	}
	if !strings.Contains(result, "Middle text") {
		t.Error("missing 'Middle text'")
	}
	if !strings.Contains(result, "End") {
		t.Error("missing 'End'")
	}
}

func TestConvertMarkdownTables_InsideCodeBlock(t *testing.T) {
	// A table inside a code block should NOT be converted
	input := "```\n| a | b |\n|---|---|\n| 1 | 2 |\n```"
	result, placeholders := convertMarkdownTables(input)

	t.Logf("Result: %s", result)
	t.Logf("Placeholders: %d", len(placeholders))

	if len(placeholders) != 0 {
		t.Errorf("expected 0 placeholders for table inside code block, got %d", len(placeholders))
	}
	if result != input {
		t.Errorf("table inside code block should pass through unchanged\ngot:  %s\nwant: %s", result, input)
	}
}
