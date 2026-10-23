package sources

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// PathPolicy lets the host restrict which files a CSV source may read (set at startup).
var PathPolicy func(path string) error

func csvType() Type {
	return Type{
		Name:  "csv",
		Label: "CSV file",
		Fields: []Field{
			{Name: "path", Label: "File path", Type: "string", Required: true, Help: "Absolute path readable by the service account, e.g. \\\\fileserver\\hr\\employees.csv"},
			{Name: "delimiter", Label: "Delimiter", Type: "string", Default: ",", Help: "Single character; use \\t for tab."},
		},
		New: func(cfg map[string]any) (Source, error) {
			delim := cfgString(cfg, "delimiter", ",")
			if delim == `\t` {
				delim = "\t"
			}
			if len([]rune(delim)) != 1 {
				return nil, errors.New("csv: delimiter must be a single character")
			}
			path := cfgString(cfg, "path", "")
			if path == "" {
				return nil, errors.New("csv: path is required")
			}
			if PathPolicy != nil {
				if err := PathPolicy(path); err != nil {
					return nil, fmt.Errorf("csv: %w", err)
				}
			}
			return &csvSource{path: path, delim: []rune(delim)[0]}, nil
		},
	}
}

type csvSource struct {
	path  string
	delim rune
}

func (c *csvSource) Fetch(ctx context.Context) ([]Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("csv: %w", err)
	}
	return ParseCSV(data, c.delim)
}

// ParseCSV parses a header-first CSV. A UTF-8 BOM is tolerated; headers are trimmed;
// short rows are padded; blank lines are skipped. Duplicate header names are an error
// because the later column would silently shadow the earlier one.
func ParseCSV(data []byte, delim rune) ([]Row, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("csv: file is empty (no header row)")
	}
	if err != nil {
		return nil, fmt.Errorf("csv: header: %w", err)
	}
	seen := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, fmt.Errorf("csv: header column %d has no name", i+1)
		}
		if seen[strings.ToLower(h)] {
			return nil, fmt.Errorf("csv: duplicate header %q", h)
		}
		seen[strings.ToLower(h)] = true
		header[i] = h
	}

	var rows []Row
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: row %d: %w", line, err)
		}
		if len(rec) > len(header) {
			return nil, fmt.Errorf("csv: row %d has %d fields but the header has %d", line, len(rec), len(header))
		}
		row := make(Row, len(header))
		for i, h := range header {
			v := ""
			if i < len(rec) {
				v = strings.TrimSpace(rec[i])
			}
			row[h] = v
		}
		rows = append(rows, row)
	}
	return rows, nil
}
