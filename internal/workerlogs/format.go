package workerlogs

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// NewFormatter returns the line format of docs/workers-logs-design.md §7.
func NewFormatter() Formatter { return formatter{} }

type formatter struct{}

// truncatedSuffix marks a record Cloudflare cut (over 256 KB; DOCS:
// https://developers.cloudflare.com/workers/observability/logs/workers-logs/).
const truncatedSuffix = " [truncated]"

// Format renders one event (design §7.2):
//
//	[<level>] <message>                       console call; continuation lines verbatim
//	[exception] <name>: <message>             then each stack line verbatim
//	[fetch] GET <url> -> 200 ok (wall 1ms, cpu 0ms) ray=<ray>
//	[scheduled] "<cron>" -> ok | [alarm] -> ok | [queue] <q> (3 messages) -> ok
//	[email] from:<a> to:<b> size:<n> -> ok | [rpc] <Entrypoint>.<method> -> ok
//	[tail] tailing <scripts> -> ok | [event] -> <outcome>
//	[notice] <message>
//
// Every line is sanitized (escapeControl). A truncated record gets " [truncated]" on its first
// line. Continuation lines follow the way wrangler prints a multi-line console message or a
// stack (the text after the first line is written as is; SOURCED:
// cloudflare/workers-sdk@3bdcd0d:packages/wrangler/src/tail/printing.ts#L126-L137).
func (formatter) Format(ev Event) []Line {
	var lines []string
	switch ev.Kind {
	case KindLog:
		level := ev.Level
		if level == "" {
			level = "log"
		}
		lines = prefixFirst("["+level+"] ", splitLines(ev.Message))
	case KindException:
		lines = formatException(ev.Exception)
	case KindInvocation:
		lines = []string{formatInvocation(ev.Invocation, ev.Message)}
	case KindNotice:
		lines = prefixFirst("[notice] ", splitLines(ev.Message))
	default:
		lines = prefixFirst("[event] ", splitLines(ev.Message))
	}
	out := make([]Line, 0, len(lines))
	for i, l := range lines {
		text := escapeControl(l)
		if i == 0 && ev.Truncated {
			text += truncatedSuffix
		}
		out = append(out, Line{Time: ev.Time, Text: text})
	}
	return out
}

func formatException(x *Exception) []string {
	if x == nil {
		return []string{"[exception]"}
	}
	head := "[exception]"
	switch {
	case x.Name != "" && x.Message != "":
		head += " " + x.Name + ": " + x.Message
	case x.Name != "":
		head += " " + x.Name
	case x.Message != "":
		head += " " + x.Message
	}
	lines := splitLines(head)
	if x.Stack != "" {
		lines = append(lines, splitLines(x.Stack)...)
	}
	return lines
}

// formatInvocation writes the summary line. extra is Event.Message: for a tail-consumer
// invocation it carries the tailed scripts (the contract's Invocation has no field for them).
func formatInvocation(inv *Invocation, extra string) string {
	if inv == nil {
		return "[event] -> unknown"
	}
	outcome := inv.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	var b strings.Builder
	switch inv.Type {
	case "fetch":
		b.WriteString("[fetch]")
		if inv.Method != "" {
			b.WriteString(" " + inv.Method)
		}
		if inv.URL != "" {
			b.WriteString(" " + inv.URL)
		}
		b.WriteString(" ->")
		if inv.Status > 0 {
			b.WriteString(" " + strconv.Itoa(inv.Status))
		}
		if inv.Outcome != "" || inv.Status <= 0 {
			b.WriteString(" " + outcome)
		}
		writeTimes(&b, inv)
		if inv.RayID != "" {
			b.WriteString(" ray=" + inv.RayID)
		}
		return b.String()
	case "scheduled":
		b.WriteString("[scheduled]")
		if inv.Cron != "" {
			b.WriteString(` "` + inv.Cron + `"`)
		}
	case "alarm":
		b.WriteString("[alarm]")
	case "queue":
		b.WriteString("[queue]")
		if inv.Queue != "" {
			b.WriteString(" " + inv.Queue)
		}
		unit := "messages"
		if inv.BatchSize == 1 {
			unit = "message"
		}
		b.WriteString(" (" + strconv.Itoa(inv.BatchSize) + " " + unit + ")")
	case "email":
		b.WriteString("[email] from:" + inv.MailFrom + " to:" + inv.RcptTo + " size:" + strconv.FormatInt(inv.RawSize, 10))
	case "rpc":
		b.WriteString("[rpc] ")
		if inv.Entrypoint != "" {
			b.WriteString(inv.Entrypoint + ".")
		}
		b.WriteString(inv.RPCMethod)
	case "tail":
		b.WriteString("[tail] tailing " + extra)
	default:
		b.WriteString("[event]")
	}
	b.WriteString(" -> " + outcome)
	writeTimes(&b, inv)
	if inv.RayID != "" {
		b.WriteString(" ray=" + inv.RayID)
	}
	return b.String()
}

func writeTimes(b *strings.Builder, inv *Invocation) {
	var parts []string
	if inv.WallTimeMs != nil {
		parts = append(parts, "wall "+strconv.FormatFloat(*inv.WallTimeMs, 'f', -1, 64)+"ms")
	}
	if inv.CPUTimeMs != nil {
		parts = append(parts, "cpu "+strconv.FormatFloat(*inv.CPUTimeMs, 'f', -1, 64)+"ms")
	}
	if len(parts) > 0 {
		b.WriteString(" (" + strings.Join(parts, ", ") + ")")
	}
}

// splitLines splits s on "\n" (a trailing "\r" of each line is dropped with it, and trailing
// newlines produce no empty last line). The empty string is one empty line.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\r\n")
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

func prefixFirst(prefix string, lines []string) []string {
	lines[0] = prefix + lines[0]
	return lines
}

// escapeControl makes text safe for an operator's terminal (design §7.2): C0 control characters
// other than tab, and DEL, become \xNN, so a request URL or a console message can't inject
// terminal escapes. Beyond the design, C1 controls (U+0080–U+009F, which some terminals treat
// as CSI and friends) become \u00NN, bytes that are not valid UTF-8 become \xNN, and characters
// that make text read differently from what it is (see deceptive) become \uNNNN.
func escapeControl(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c < 0x20 && c != '\t' || c == 0x7f {
				b.WriteString(`\x`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		case r >= 0x80 && r <= 0x9f || deceptive(r):
			b.WriteString(`\u`)
			b.WriteByte(hex[r>>12&0xf])
			b.WriteByte(hex[r>>8&0xf])
			b.WriteByte(hex[r>>4&0xf])
			b.WriteByte(hex[r&0xf])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// deceptive reports invisible or reordering characters: the bidi embeddings, overrides and
// isolates (U+202A–U+202E, U+2066–U+2069; "Trojan Source"), the line and paragraph separators
// (U+2028, U+2029) and the zero-width characters (U+200B–U+200F, U+FEFF).
func deceptive(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
		r == 0x2028, r == 0x2029, r == 0xfeff:
		return true
	}
	return false
}
