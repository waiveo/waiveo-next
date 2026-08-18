package store

import (
	"fmt"
	"strings"
	"unicode"
)

// schemasql.go reads a CREATE TABLE statement the way the ALTER needs it read:
// as a list of column DECLARATIONS, verbatim.
//
// # Why the declaration text is needed at all
//
// The additive migration (schemamigrate.go) used to render the column it was
// about to add from `PRAGMA table_info`, which reports a column's name, type,
// NOT NULL, DEFAULT and primary-key position — and NOTHING else. A declaration
// carrying UNIQUE, COLLATE, CHECK, REFERENCES or GENERATED ALWAYS AS came back
// stripped of all of it, so the retrofitted column was a WEAKER column than the
// same DDL gives a fresh install, silently and permanently: the comparison that
// runs on every subsequent boot reads through the same blind pragma and reports
// the two as identical forever. A fleet split between boxes that enforce a
// constraint and boxes that do not, with nothing anywhere saying so, is the #194
// defect one level up.
//
// So the declaration is taken from the DDL's own text — read back out of the
// throwaway model database's `sqlite_schema.sql`, which SQLite stores verbatim,
// comments and all — and appended to the ALTER exactly as written. A migrated
// column is then declared with the same words a fresh one is, and anything
// SQLite refuses to retrofit (ADD COLUMN cannot take UNIQUE) it refuses on the
// real statement rather than on a laundered one.
//
// # Why this is not a SQL parser
//
// It does not interpret SQL; it finds the top-level commas. It tracks string
// literals, quoted identifiers and comments only so a comma inside one is not
// mistaken for a separator, and it decides "column or table constraint" from the
// leading keyword. Everything it hands back is the source text, unaltered except
// for whitespace collapsing outside quotes.
//
// It is also checked rather than trusted: TestEveryDeclaredTableIsReadableAsColumns
// asserts, for every table this build declares, that the column names this file
// finds are exactly the ones SQLite reports through PRAGMA table_xinfo, in the
// same order. A DDL spelling this code cannot read fails CI in the change that
// introduces it, not on a box.

// normalizeSQL rewrites SQL text into a single-line form — comments removed,
// runs of whitespace collapsed to one space — and returns a parallel mask
// marking every byte that sits inside a string literal or a quoted identifier.
//
// The mask is the point. Depth counting, comma splitting and keyword matching
// all have to ignore anything inside quotes, and a `DEFAULT '(a,b)'` or a column
// named "check" is exactly the input that turns a naive scan into a wrong
// answer.
func normalizeSQL(s string) (string, []bool) {
	var (
		out    strings.Builder
		quoted []bool
	)
	emit := func(r byte, inQuote bool) {
		out.WriteByte(r)
		quoted = append(quoted, inQuote)
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			// A line comment stands in for a single space: the DDL carries them
			// between columns (packs.enabled has two lines of them) and dropping
			// them outright could weld two tokens together.
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if out.Len() > 0 && out.String()[out.Len()-1] != ' ' {
				emit(' ', false)
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
			if out.Len() > 0 && out.String()[out.Len()-1] != ' ' {
				emit(' ', false)
			}
		case c == '\'' || c == '"' || c == '`':
			// A quoted span, copied byte for byte. The doubled-quote escape needs
			// no special handling: the closing quote of the first half is read as
			// a close and the opening quote of the second half as a re-open, and
			// since both are copied verbatim and both are marked quoted, the text
			// and the mask come out the same either way.
			emit(c, true)
			i++
			for i < len(s) {
				emit(s[i], true)
				if s[i] == c {
					i++
					break
				}
				i++
			}
		case c == '[':
			emit(c, true)
			i++
			for i < len(s) {
				emit(s[i], true)
				if s[i] == ']' {
					i++
					break
				}
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
				i++
			}
			if out.Len() > 0 && out.String()[out.Len()-1] != ' ' {
				emit(' ', false)
			}
		default:
			emit(c, false)
			i++
		}
	}
	// Trim by INDEX rather than through strings.TrimSpace, so the mask stays
	// aligned with the text byte for byte — and so a quoted span that begins or
	// ends with a real space (`DEFAULT ' '`) is never trimmed away.
	text := out.String()
	lead := 0
	for lead < len(text) && text[lead] == ' ' && !quoted[lead] {
		lead++
	}
	trail := len(text)
	for trail > lead && text[trail-1] == ' ' && !quoted[trail-1] {
		trail--
	}
	return text[lead:trail], quoted[lead:trail]
}

// createTableItems splits a CREATE TABLE statement's parenthesized body into its
// top-level items — one per column declaration and one per table constraint — in
// declaration order, each normalized and trimmed.
func createTableItems(createSQL string) ([]string, error) {
	text, quoted := normalizeSQL(createSQL)
	open := -1
	for i := 0; i < len(text); i++ {
		if text[i] == '(' && !quoted[i] {
			open = i
			break
		}
	}
	if open < 0 {
		return nil, fmt.Errorf("store: %.60q has no column list", createSQL)
	}
	depth := 0
	start := open + 1
	var items []string
	for i := open; i < len(text); i++ {
		if quoted[i] {
			continue
		}
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				items = append(items, strings.TrimSpace(text[start:i]))
				return items, nil
			}
		case ',':
			if depth == 1 {
				items = append(items, strings.TrimSpace(text[start:i]))
				start = i + 1
			}
		}
	}
	return nil, fmt.Errorf("store: %.60q has an unterminated column list", createSQL)
}

// tableConstraintKeywords are the words a top-level item starts with when it is
// a TABLE constraint rather than a column. Everything else in that position is a
// column name.
var tableConstraintKeywords = map[string]bool{
	"CONSTRAINT": true,
	"PRIMARY":    true,
	"UNIQUE":     true,
	"CHECK":      true,
	"FOREIGN":    true,
}

// leadingIdent reads the identifier an item starts with, honouring SQLite's four
// quoting styles, and reports whether it was quoted (a quoted leading token is
// always a column name — `"check" TEXT` is a column, `CHECK (...)` is not).
func leadingIdent(item string) (name string, wasQuoted bool) {
	if item == "" {
		return "", false
	}
	switch item[0] {
	case '"', '`':
		q := item[0]
		var b strings.Builder
		for i := 1; i < len(item); i++ {
			if item[i] == q {
				if i+1 < len(item) && item[i+1] == q {
					b.WriteByte(q)
					i++
					continue
				}
				return b.String(), true
			}
			b.WriteByte(item[i])
		}
		return b.String(), true
	case '[':
		if end := strings.IndexByte(item, ']'); end > 0 {
			return item[1:end], true
		}
		return "", true
	}
	end := strings.IndexFunc(item, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ','
	})
	if end < 0 {
		return item, false
	}
	return item[:end], false
}

// tableDecls splits a CREATE TABLE statement into its column declarations (by
// column name, and in order) and its table-level constraint items.
func tableDecls(createSQL string) (decls map[string]string, order []string, constraints []string, err error) {
	items, err := createTableItems(createSQL)
	if err != nil {
		return nil, nil, nil, err
	}
	decls = make(map[string]string, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		name, wasQuoted := leadingIdent(item)
		if !wasQuoted && tableConstraintKeywords[strings.ToUpper(name)] {
			constraints = append(constraints, item)
			continue
		}
		if name == "" {
			return nil, nil, nil, fmt.Errorf("store: cannot read a column name from %.60q", item)
		}
		decls[name] = item
		order = append(order, name)
	}
	return decls, order, constraints, nil
}

// declHasKeyword reports whether a column declaration carries kw as a bare word
// outside any quoted span — the reading that has to be right before a column is
// planned as an ordinary addition, because UNIQUE (and only UNIQUE, among the
// clauses PRAGMA table_info cannot see) makes the ALTER something SQLite will
// refuse.
func declHasKeyword(decl, kw string) bool {
	text, quoted := normalizeSQL(decl)
	upper := strings.ToUpper(text)
	for i := 0; i+len(kw) <= len(upper); i++ {
		if quoted[i] || upper[i:i+len(kw)] != kw {
			continue
		}
		if i > 0 && isIdentByte(upper[i-1]) {
			continue
		}
		if j := i + len(kw); j < len(upper) && isIdentByte(upper[j]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// canonicalConstraint reduces a TABLE constraint to the form two builds can be
// compared on: keywords upper-cased, spacing around punctuation removed, quoted
// text left exactly as it was written.
//
// It exists so that `PRIMARY KEY (a, b)` and `PRIMARY KEY(a,b)` — the same
// constraint, spelled by two builds' formatting — do not read as drift on every
// boot of every box forever. Only the COMPARISON is canonicalized; what an
// operator is shown is the text as the DDL wrote it.
func canonicalConstraint(item string) string {
	text, quoted := normalizeSQL(item)
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quoted[i] {
			b.WriteByte(c)
			continue
		}
		if c == ' ' {
			// Drop a space that sits beside punctuation; keep the ones that
			// separate words.
			prevPunct := b.Len() > 0 && isPunct(b.String()[b.Len()-1])
			nextPunct := i+1 < len(text) && !quoted[i+1] && isPunct(text[i+1])
			if prevPunct || nextPunct {
				continue
			}
			b.WriteByte(' ')
			continue
		}
		if c >= 'a' && c <= 'z' {
			b.WriteByte(c - ('a' - 'A'))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isPunct(c byte) bool { return c == '(' || c == ')' || c == ',' }
