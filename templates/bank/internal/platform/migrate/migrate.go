// Package migrate applies DDL text to an existing database.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Run executes the DDL text (statements are separated by semicolons and executed one by one).
func Run(ctx context.Context, db *sql.DB, ddl string) error {
	for _, stmt := range SplitStatements(ddl) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: 执行失败 %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// SplitStatements splits SQL statements by semicolons.
//
// It is dollar-quote aware: a semicolon inside a Postgres dollar-quoted body
// ($$ ... $$ or $tag$ ... $tag$) is NOT treated as a statement separator, so
// PL/pgSQL function and trigger bodies (which routinely contain semicolons)
// round-trip as a single statement. Single-quoted string literals and
// `-- ...` line comments are also skipped over so semicolons inside them are
// inert. Empty statements are dropped.
//
// The parser is intentionally minimal: it tracks dollar-quoting and
// single-quoting only, which is sufficient for the project's DDL files.
func SplitStatements(sqlText string) []string {
	var (
		out      []string
		buf      strings.Builder
		n        = len(sqlText)
		i        = 0
		inSingle = false
	)
	flush := func() {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			out = append(out, s)
		}
		buf.Reset()
	}
	for i < n {
		c := sqlText[i]
		// Single-quote handling. Standard SQL escapes a quote inside a string
		// by doubling it (''); toggling on each ' is the equivalent rule here.
		if c == '\'' {
			inSingle = !inSingle
			buf.WriteByte(c)
			i++
			continue
		}
		if !inSingle {
			// Line comment -- ... \n: copy through the newline verbatim.
			if c == '-' && i+1 < n && sqlText[i+1] == '-' {
				j := strings.IndexByte(sqlText[i:], '\n')
				if j < 0 {
					buf.WriteString(sqlText[i:])
					i = n
				} else {
					buf.WriteString(sqlText[i : i+j+1])
					i += j + 1
				}
				continue
			}
			// Dollar-quoted body: $tag$ ... $tag$. The opening tag must begin
			// at the current position; once matched, copy everything through
			// the matching close verbatim so embedded ';' is inert.
			if tag, ok := readDollarTag(sqlText, i); ok {
				closeIdx := indexAt(sqlText, tag, i+len(tag))
				if closeIdx < 0 {
					// Unterminated dollar quote: copy remainder verbatim.
					buf.WriteString(sqlText[i:])
					i = n
				} else {
					end := closeIdx + len(tag)
					buf.WriteString(sqlText[i:end])
					i = end
				}
				continue
			}
			if c == ';' {
				flush()
				i++
				continue
			}
		}
		buf.WriteByte(c)
		i++
	}
	flush()
	return out
}

// readDollarTag reports whether sql[at:] begins with a Postgres dollar-quote
// opening tag ($tag$ where tag is empty or an identifier starting with a
// letter/underscore). It returns the tag (e.g. "$$", "$body$") and ok=true
// when one is present. The match mirrors PostgreSQL's lexer: a non-empty tag
// must start with a letter or underscore; subsequent characters may include
// digits.
func readDollarTag(sql string, at int) (string, bool) {
	n := len(sql)
	if at >= n || sql[at] != '$' {
		return "", false
	}
	i := at + 1
	for i < n {
		c := sql[i]
		if c == '$' {
			tag := sql[at : i+1]
			// Validate the tag body when non-empty: first char letter/_.
			if len(tag) > 2 {
				first := tag[1]
				if !((first >= 'a' && first <= 'z') ||
					(first >= 'A' && first <= 'Z') ||
					first == '_') {
					return "", false
				}
			}
			return tag, true
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			i++
		case i > at+1 && c >= '0' && c <= '9':
			// Digits allowed only after the first tag char.
			i++
		default:
			return "", false
		}
	}
	return "", false
}

// indexAt returns the index of the first occurrence of substr in s at or after
// offset at, or -1. It is a thin offset-aware wrapper over strings.Index.
func indexAt(s, substr string, at int) int {
	if at >= len(s) {
		return -1
	}
	idx := strings.Index(s[at:], substr)
	if idx < 0 {
		return -1
	}
	return at + idx
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
