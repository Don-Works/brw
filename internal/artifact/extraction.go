package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

type ExtractionSpec struct {
	Source         string   `json:"source"`
	Section        string   `json:"section,omitempty"`
	Caption        string   `json:"caption,omitempty"`
	Headers        []string `json:"headers,omitempty"`
	ExpectedSource string   `json:"expected_source,omitempty"`
	Fields         []string `json:"fields,omitempty"`
	MaxChars       int      `json:"max_chars,omitempty"`
	MaxRows        int      `json:"max_rows,omitempty"`
	MaxColumns     int      `json:"max_columns,omitempty"`
	MaxCellChars   int      `json:"max_cell_chars,omitempty"`
	MaxBytes       int      `json:"max_bytes"`
}

type ExtractionProvenance struct {
	Origin         string    `json:"origin"`
	DocumentID     string    `json:"document_id"`
	CapturedAt     time.Time `json:"captured_at"`
	Source         string    `json:"source"`
	Section        string    `json:"section,omitempty"`
	Start          int       `json:"start,omitempty"`
	End            int       `json:"end,omitempty"`
	Caption        string    `json:"caption,omitempty"`
	Headers        []string  `json:"headers,omitempty"`
	ExpectedSource string    `json:"expected_source,omitempty"`
	Fields         []string  `json:"fields,omitempty"`
}

type ExtractionResult struct {
	Name       string               `json:"name"`
	Data       any                  `json:"data"`
	Provenance ExtractionProvenance `json:"provenance"`
}

var extractionName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var structuredFields = []string{"title", "type", "name", "price", "currency", "availability", "rating", "reviewCount", "brand"}

func ValidateExtraction(name string, spec *ExtractionSpec) error {
	if !extractionName.MatchString(name) {
		return errors.New("extraction name must be a lowercase identifier of at most 64 characters")
	}
	if spec == nil {
		return errors.New("extraction_json requires extract")
	}
	if spec.MaxBytes < 1 || spec.MaxBytes > 256<<10 {
		return errors.New("extraction max_bytes must be 1..262144")
	}
	for _, value := range append([]string{spec.Section, spec.Caption, spec.ExpectedSource}, append(append([]string{}, spec.Headers...), spec.Fields...)...) {
		if !utf8.ValidString(value) || len(value) > 1000 || strings.Contains(value, "${") || strings.Contains(value, "secret://") {
			return errors.New("extraction selectors must be bounded literal strings without templates or credential references")
		}
	}
	switch spec.Source {
	case "section":
		if strings.TrimSpace(spec.Section) == "" || spec.MaxChars < 1 || spec.MaxChars > 20000 {
			return errors.New("section extraction requires section and max_chars 1..20000")
		}
		if spec.Caption != "" || len(spec.Headers) > 0 || spec.ExpectedSource != "" || len(spec.Fields) > 0 || spec.MaxRows != 0 || spec.MaxColumns != 0 || spec.MaxCellChars != 0 {
			return errors.New("section extraction received fields for another source")
		}
	case "table":
		if strings.TrimSpace(spec.Caption) == "" || spec.MaxRows < 1 || spec.MaxRows > 40 || spec.MaxColumns < 1 || spec.MaxColumns > 100 || spec.MaxCellChars < 1 || spec.MaxCellChars > 2000 {
			return errors.New("table extraction requires caption, max_rows 1..40, max_columns 1..100, max_cell_chars 1..2000")
		}
		if len(spec.Headers) > spec.MaxColumns || spec.Section != "" || spec.ExpectedSource != "" || len(spec.Fields) > 0 || spec.MaxChars != 0 {
			return errors.New("table extraction received incompatible fields")
		}
	case "structured":
		if !slices.Contains([]string{"json_document", "next_data", "json_ld", "microdata", "meta", "inline_script"}, spec.ExpectedSource) || len(spec.Fields) < 1 || len(spec.Fields) > len(structuredFields) {
			return errors.New("structured extraction requires expected_source and normalized fields")
		}
		seen := map[string]bool{}
		for _, field := range spec.Fields {
			if !slices.Contains(structuredFields, field) || seen[field] {
				return errors.New("structured fields must be distinct normalized scalar fields; raw data is unavailable")
			}
			seen[field] = true
		}
		if spec.Section != "" || spec.Caption != "" || len(spec.Headers) > 0 || spec.MaxChars != 0 || spec.MaxRows != 0 || spec.MaxColumns != 0 || spec.MaxCellChars != 0 {
			return errors.New("structured extraction received fields for another source")
		}
	default:
		return errors.New("extraction source must be section, table, or structured")
	}
	return nil
}

func (s *Service) captureExtraction(ctx context.Context, opts CaptureOptions, put PutOptions, continuity *recipeCaptureContinuity) (Meta, error) {
	if browser.IsSensitiveAction(ctx) {
		return Meta{}, errors.New("extraction is unavailable for a sensitive action")
	}
	if continuity == nil {
		provider, ok := s.browser.(browser.DocumentIdentityProvider)
		if !ok {
			return Meta{}, errors.New("extraction requires main-document identity support")
		}
		identity, err := provider.DocumentIdentity(ctx)
		if err != nil {
			return Meta{}, errors.New("could not verify extraction document")
		}
		continuity = &recipeCaptureContinuity{provider: provider, start: identity, allowed: []string{identity.Origin}}
		if err := continuity.validate(identity); err != nil {
			return Meta{}, err
		}
	}
	spec := opts.Extract
	result := ExtractionResult{Name: opts.Name, Provenance: ExtractionProvenance{Origin: continuity.start.Origin, DocumentID: continuity.start.ID, CapturedAt: time.Now().UTC(), Source: spec.Source}}
	if spec.Source == "structured" {
		data, err := s.browser.ReadData(ctx)
		if err != nil {
			return Meta{}, err
		}
		if err := validateExtractionURL(data.URL, continuity.start.Origin); err != nil {
			return Meta{}, err
		}
		if data.Source != spec.ExpectedSource {
			return Meta{}, errors.New("structured extraction source does not match expected_source")
		}
		normalized := normalizedStructured(data)
		selected := map[string]string{}
		for _, field := range spec.Fields {
			value := normalized[field]
			if strings.TrimSpace(value) == "" {
				return Meta{}, fmt.Errorf("required normalized field %s is missing", field)
			}
			selected[field] = value
		}
		result.Data = selected
		result.Provenance.ExpectedSource = data.Source
		result.Provenance.Fields = append([]string(nil), spec.Fields...)
	} else {
		read, err := s.browser.Read(ctx)
		if err != nil {
			return Meta{}, err
		}
		if err := validateExtractionURL(read.URL, continuity.start.Origin); err != nil {
			return Meta{}, err
		}
		if read.MainTruncated || read.HeadingsTruncated || read.Section != "" || (read.MainTotalChars != 0 && read.MainTotalChars != len([]rune(read.Main))) {
			return Meta{}, errors.New("extraction requires an unwindowed complete source read")
		}
		if spec.Source == "section" {
			if !read.SectionsAnchored {
				return Meta{}, errors.New("section extraction requires headings anchored to unique text inside main content")
			}
			if len(utf16.Encode([]rune(read.Main))) >= 100000 {
				return Meta{}, errors.New("section source may have reached the browser read limit")
			}
			matches := 0
			var matched readability.Heading
			for _, heading := range read.Headings {
				if heading.Text == spec.Section {
					matches++
					matched = heading
					if heading.Offset == nil || *heading.Offset < 0 {
						return Meta{}, errors.New("section heading is not addressable")
					}
				}
			}
			if matches != 1 {
				return Meta{}, fmt.Errorf("section extraction matched %d headings; requires exactly one", matches)
			}
			start, end := *matched.Offset, len([]rune(read.Main))
			if start >= end {
				return Meta{}, errors.New("section offset is outside source prose")
			}
			for _, heading := range read.Headings {
				if heading.Offset != nil && *heading.Offset > start && *heading.Offset < end && heading.Level <= matched.Level {
					end = *heading.Offset
				}
			}
			span := readability.SectionSpan{Start: start, End: end}
			data := string([]rune(read.Main)[span.Start:span.End])
			if len([]rune(data)) > spec.MaxChars {
				return Meta{}, errors.New("section exceeds max_chars")
			}
			result.Data = data
			result.Provenance.Section = spec.Section
			result.Provenance.Start = span.Start
			result.Provenance.End = span.End
		} else {
			if !read.TablesComplete || read.TablesTruncated {
				return Meta{}, errors.New("table source may have reached the browser table limit")
			}
			selected := []readability.Table{}
			for _, table := range read.Tables {
				if table.Caption == spec.Caption && (len(spec.Headers) == 0 || reflect.DeepEqual(table.Headers, spec.Headers)) {
					selected = append(selected, table)
				}
			}
			if len(selected) != 1 {
				return Meta{}, fmt.Errorf("table extraction matched %d tables; requires exactly one", len(selected))
			}
			table := selected[0]
			if table.Truncated || len(table.Rows) > spec.MaxRows || len(table.Headers) > spec.MaxColumns {
				return Meta{}, errors.New("table exceeds its row or column budget, or the browser row limit")
			}
			for _, row := range append([][]string{table.Headers}, table.Rows...) {
				if len(row) > spec.MaxColumns {
					return Meta{}, errors.New("table exceeds max_columns")
				}
				for _, cell := range row {
					if len([]rune(cell)) > spec.MaxCellChars {
						return Meta{}, errors.New("table cell exceeds max_cell_chars")
					}
				}
			}
			result.Data = table
			result.Provenance.Caption = table.Caption
			result.Provenance.Headers = append([]string(nil), table.Headers...)
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return Meta{}, err
	}
	if len(data) > spec.MaxBytes {
		return Meta{}, errors.New("extraction output exceeds max_bytes")
	}
	if err := continuity.verify(ctx); err != nil {
		return Meta{}, err
	}
	put.MIMEType = "application/json"
	put.SourceHash = sourceHash(continuity.start.Origin, continuity.start.ID)
	meta, err := s.store.PutContext(ctx, put, bytes.NewReader(data))
	if err != nil {
		return Meta{}, err
	}
	if err := continuity.verify(ctx); err != nil {
		return Meta{}, errors.Join(err, s.store.Delete(meta.ID))
	}
	return meta, nil
}

func validateExtractionURL(raw, origin string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Scheme+"://"+parsed.Host != origin {
		return errors.New("extraction source URL does not match the pinned origin")
	}
	return nil
}

func normalizedStructured(data snapshot.StructuredData) map[string]string {
	return map[string]string{"title": data.Title, "type": data.Type, "name": data.Name, "price": data.Price, "currency": data.Currency, "availability": data.Availability, "rating": data.Rating, "reviewCount": data.ReviewCount, "brand": data.Brand}
}
