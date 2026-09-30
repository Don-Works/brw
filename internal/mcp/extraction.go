package mcp

func extractionSchema() map[string]any {
	return object(map[string]any{
		"source":          stringEnumSchema("Deterministic extraction source; no raw embedded JSON or inferred values.", "section", "table", "structured"),
		"section":         stringSchema("Section only: exact unique heading within anchored main content."),
		"caption":         stringSchema("Table only: exact unique caption."),
		"headers":         map[string]any{"type": "array", "items": stringSchema("Exact header text."), "description": "Table only: optional exact ordered headers to disambiguate caption."},
		"expected_source": stringEnumSchema("Structured only: required source reported by brw_read_data.", "json_document", "next_data", "json_ld", "microdata", "meta", "inline_script"),
		"fields":          map[string]any{"type": "array", "items": stringEnumSchema("Required normalized scalar field.", "title", "type", "name", "price", "currency", "availability", "rating", "reviewCount", "brand"), "description": "Structured only: one or more distinct fields; every field must be present."},
		"max_chars":       integerSchema("Section only: required complete section budget, 1..20000 runes."),
		"max_rows":        integerSchema("Table only: required row budget, 1..40."),
		"max_columns":     integerSchema("Table only: required column budget, 1..100."),
		"max_cell_chars":  integerSchema("Table only: required cell budget, 1..2000 runes."),
		"max_bytes":       integerSchema("Required total UTF-8 JSON artifact budget including provenance, 1..262144. Overflow fails without storing output."),
	}, []string{"source", "max_bytes"})
}
