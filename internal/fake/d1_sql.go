package fake

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A minimal, deterministic stand-in for D1 SQL execution: constant SELECT statements
// (`SELECT 1 AS one, 'x', ?1, NULL`) with no FROM clause. Anything else (tables, DDL, DML,
// functions, operators) is reported as unsupported: the query route answers 400/99999 (a 4XX,
// because the spec declares no other error status for the route).
//
// flarefake deliberately has no SQL engine: the pure-Go SQLite driver would pull a large
// transpiled C runtime into the operator's module for a surface the operator does not use.
//
// Column names follow SQLite's rules for a result column: the alias if given, else the
// expression text as written. D1 runs SQLite, so this is what the API returns too, but the
// response has not been recorded (UNVERIFIED).

type d1Column struct {
	name  string
	value any
}

// evalConstantSelect evaluates one statement, or returns an error naming what is unsupported.
// params are the query's bound parameters (?N is 1-based; bare ? takes the next one).
func evalConstantSelect(stmt string, params []any) ([]d1Column, error) {
	l := &sqlLexer{src: stmt}
	if kw := l.word(); !strings.EqualFold(kw, "select") {
		return nil, fmt.Errorf("only constant SELECT statements are emulated, got %q", firstWords(stmt))
	}
	var cols []d1Column
	next := 0
	for {
		l.space()
		start := l.pos
		v, err := l.literal(params, &next)
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(l.src[start:l.pos])
		l.space()
		name := text
		save := l.pos
		switch w := l.word(); {
		case strings.EqualFold(w, "as"):
			l.space()
			if name = l.ident(); name == "" {
				return nil, fmt.Errorf("expected a column alias after AS")
			}
		case w != "" && !strings.EqualFold(w, "from"):
			name = w // SELECT 1 one
		default:
			l.pos = save
		}
		cols = append(cols, d1Column{name: name, value: v})
		l.space()
		if l.peek() == ',' {
			l.pos++
			continue
		}
		break
	}
	l.space()
	if l.pos < len(l.src) {
		return nil, fmt.Errorf("only constant SELECT statements (no FROM, WHERE or expressions) are emulated, near %q", firstWords(l.src[l.pos:]))
	}
	return cols, nil
}

// splitSQL splits on top-level semicolons (as countSQLStatements counts them) and drops empty
// statements.
func splitSQL(sql string) []string {
	var out []string
	start := 0
	for i := 0; i < len(sql); i++ {
		switch ch := sql[i]; {
		case ch == '\'' || ch == '"' || ch == '`':
			if end := strings.IndexByte(sql[i+1:], ch); end >= 0 {
				i += end + 1
			} else {
				i = len(sql)
			}
		case ch == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = len(sql)
			}
		case ch == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
				i += end + 3
			} else {
				i = len(sql)
			}
		case ch == ';':
			if seg := sql[start:i]; stripSQLComments(seg) != "" {
				out = append(out, strings.TrimSpace(seg))
			}
			start = i + 1
		}
	}
	if seg := sql[min(start, len(sql)):]; stripSQLComments(seg) != "" {
		out = append(out, strings.TrimSpace(seg))
	}
	return out
}

func stripSQLComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*"):
			if end := strings.Index(s, "*/"); end >= 0 {
				s = s[end+2:]
			} else {
				return ""
			}
		default:
			return s
		}
	}
}

func firstWords(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}

type sqlLexer struct {
	src string
	pos int
}

func (l *sqlLexer) peek() byte {
	if l.pos < len(l.src) {
		return l.src[l.pos]
	}
	return 0
}

// space skips whitespace and comments.
func (l *sqlLexer) space() {
	for l.pos < len(l.src) {
		switch {
		case unicode.IsSpace(rune(l.src[l.pos])):
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "--"):
			if nl := strings.IndexByte(l.src[l.pos:], '\n'); nl >= 0 {
				l.pos += nl + 1
			} else {
				l.pos = len(l.src)
			}
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			if end := strings.Index(l.src[l.pos+2:], "*/"); end >= 0 {
				l.pos += end + 4
			} else {
				l.pos = len(l.src)
			}
		default:
			return
		}
	}
}

func isIdentByte(b byte, first bool) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || !first && b >= '0' && b <= '9'
}

// word reads a bare identifier or keyword ("" if none).
func (l *sqlLexer) word() string {
	l.space()
	start := l.pos
	for l.pos < len(l.src) && isIdentByte(l.src[l.pos], l.pos == start) {
		l.pos++
	}
	return l.src[start:l.pos]
}

// ident reads a bare or quoted ("x", `x`, [x]) identifier.
func (l *sqlLexer) ident() string {
	switch q := l.peek(); q {
	case '"', '`', '[':
		end := q
		if q == '[' {
			end = ']'
		}
		i := strings.IndexByte(l.src[l.pos+1:], end)
		if i < 0 {
			return ""
		}
		name := l.src[l.pos+1 : l.pos+1+i]
		l.pos += i + 2
		return name
	}
	return l.word()
}

// literal reads one constant: a number, a string, NULL, TRUE/FALSE or a parameter.
func (l *sqlLexer) literal(params []any, next *int) (any, error) {
	l.space()
	switch c := l.peek(); {
	case c == '\'':
		var b strings.Builder
		for i := l.pos + 1; i < len(l.src); i++ {
			if l.src[i] == '\'' {
				if i+1 < len(l.src) && l.src[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				l.pos = i + 1
				return b.String(), nil
			}
			b.WriteByte(l.src[i])
		}
		return nil, fmt.Errorf("unterminated string literal")
	case c == '?':
		l.pos++
		start := l.pos
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		idx := *next
		if start < l.pos {
			n, _ := strconv.Atoi(l.src[start:l.pos])
			idx = n - 1
		}
		*next = idx + 1
		if idx < 0 || idx >= len(params) {
			return nil, nil // SQLite binds a missing parameter as NULL
		}
		return params[idx], nil
	case c == '-' || c == '+' || c == '.' || c >= '0' && c <= '9':
		start := l.pos
		l.pos++
		for l.pos < len(l.src) && (l.src[l.pos] >= '0' && l.src[l.pos] <= '9' || strings.IndexByte(".eE", l.src[l.pos]) >= 0 ||
			(l.src[l.pos] == '-' || l.src[l.pos] == '+') && strings.IndexByte("eE", l.src[l.pos-1]) >= 0) {
			l.pos++
		}
		text := l.src[start:l.pos]
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return i, nil
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return f, nil
		}
		return nil, fmt.Errorf("unsupported number %q", text)
	default:
		save := l.pos
		switch w := l.word(); strings.ToLower(w) {
		case "null":
			return nil, nil
		case "true":
			return int64(1), nil
		case "false":
			return int64(0), nil
		default:
			l.pos = save
			return nil, fmt.Errorf("only constants (numbers, strings, NULL, TRUE, FALSE, parameters) are emulated, near %q", firstWords(l.src[l.pos:]))
		}
	}
}
