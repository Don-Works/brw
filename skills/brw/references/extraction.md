# Named recipe extraction

A recipe can navigate to the right page, wait for readiness, verify its state, and capture a named section, table, or set of normalized structured fields. Extraction runs deterministically in brw; it does not call an LLM or guess a selector. Definitions can live in repository `.brw/recipes/` files or come from an optional recipe provider. The extraction contract is independent of the registry backend.

Use the existing `capture` action with `kind:"extraction_json"`. A capture without `name` uses its step ID; output names must be unique lowercase identifiers, up to 64 characters. The run result's `outputs` maps names to artifact metadata handles. The existing `artifacts` array also contains those handles. Captured bytes stay in the private browser-host artifact store and are retrieved through bounded `brw_artifact_read` windows.

```json
{
  "id": "summary",
  "action": "capture",
  "capture": {
    "kind": "extraction_json",
    "name": "summary",
    "extract": {
      "source": "section",
      "section": "Results",
      "max_chars": 2000,
      "max_bytes": 8192
    }
  }
}
```

Each stored JSON output contains `name`, `data`, and `provenance`. Provenance records the observed origin, document identity, capture time, source method, and resolved heading span, table caption/headers, or structured source/fields. It excludes the page URL's query/fragment, URL credentials, and page title. The surrounding recipe result records recipe ID, version and digest; the output-name mapping links that run to each artifact. Source provenance describes what was observed, not independent verification of the site's business facts.

All source-specific bounds and `max_bytes` are explicit. `max_bytes` limits the complete UTF-8 JSON artifact, including provenance, to 1–262144 bytes. Overflow fails and stores no output; extraction does not silently return a preview.

| Source | Required selection and limits | Data |
| --- | --- | --- |
| `section` | Exact heading `section`; `max_chars` 1–20000 runes | Complete heading span through the next heading of equal or shallower level |
| `table` | Exact `caption`; optional exact ordered `headers`; `max_rows` 1–40, `max_columns` 1–100, `max_cell_chars` 1–2000 runes | Caption, headers and complete rows |
| `structured` | `expected_source`; nonempty distinct `fields` | Object containing every requested normalized scalar field |

Table example:

```json
{"kind":"extraction_json","name":"quarterly","extract":{"source":"table","caption":"Quarterly","headers":["Quarter","Revenue"],"max_rows":4,"max_columns":2,"max_cell_chars":100,"max_bytes":8192}}
```

Structured example:

```json
{"kind":"extraction_json","name":"product","extract":{"source":"structured","expected_source":"json_ld","fields":["price","currency"],"max_bytes":4096}}
```

Structured sources are the values reported by `brw_read_data`: `json_document`, `next_data`, `json_ld`, `microdata`, `meta`, and `inline_script`. Available fields are `title`, `type`, `name`, `price`, `currency`, `availability`, `rating`, `reviewCount`, and `brand`. This extracts brw's existing normalized result; it is not arbitrary JSON-schema extraction, raw embedded JSON access, or an assertion that all embedded records agree. A source mismatch or missing requested field fails.

Section/table selection requires exactly one match. Templates and credential references are forbidden in output names and extraction selectors. Put navigation, waits and assertions before capture when the page must reach a particular state. Missing or ambiguous selection, incompatible fields, unsupported source completeness, overflow, or origin/document drift fails. No output handle is returned for failed persistence, and artifacts written during document drift are deleted.

Section extraction requires the reader's positive `sections_anchored` evidence: actual headings inside the selected main content must have unique text anchors, and the reader must not have switched to its fallback or clipped main text. Repeated heading text appearing in prose can therefore cause a conservative refusal. Table extraction requires positive `tables_complete` evidence and rejects table/row truncation. The native reader caps its inventory at 20 visible tables and 40 raw rows per table; an overflow is reported before empty-row filtering. Older readers lacking these markers fail closed. Direct CDP and extension bridge share the reader; upstream MCP capture delegates to the browser host so extraction bytes remain there.

Recipes combining extraction with declared secret inputs or credential-reference steps are refused before step one. This preserves immediate credential wiping instead of retaining credential values to sanitize later page captures. Use a signed-in profile-session recipe without runtime secrets for authenticated extraction. Sensitive-action contexts are also refused. Extraction has the existing artifact privacy/retention/encryption policy; it is not a universal detector of unknown credentials or sensitive page content, and `redaction` remains a label rather than a text sanitizer.

The same capture specification works directly through `brw_artifact_capture`, with an explicit `name`, without introducing a new tool or execution language. Old recipe captures omit the new fields and retain their serialized digests and behavior.
