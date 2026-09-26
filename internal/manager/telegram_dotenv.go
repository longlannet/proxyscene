package manager

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func dotenvDeclaredKeys(raw []byte) []string {
	return declaredDotEnvKeys(raw, false)
}

func openClawDotEnvDeclaredKeys(raw []byte) []string {
	return declaredDotEnvKeys(raw, true)
}

func normalizeDotEnvLineEndings(content string) string {
	return strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
}

// Python's Unicode \s includes U+001C..U+001F in addition to Unicode White_Space;
// ECMAScript's \s also includes the BOM. Use their union for conservative header
// scanning: extra rejection is preferable to silently accepting a route selector.
func dotEnvSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= '\x1c' && r <= '\x1f' || r == '\ufeff'
}

func validateDotEnvText(raw []byte) error {
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("dotenv 必须是无 NUL 的有效 UTF-8，无法证明其中没有运行配置选择器")
	}
	return nil
}

// declaredDotEnvKeys scans declaration headers, not values. Python dotenv keys
// are single-quoted strings or [^=#\s]+; Node dotenv keys are ASCII [\w.-]+,
// with either whitespace + '=' or an immediate ':' followed by whitespace.
// Values may span lines, so this deliberately also reports plausible headers
// inside multiline values and bare keys (Node permits a newline before '=')
// instead of treating ambiguous input as proof that no selector is present.
func declaredDotEnvKeys(raw []byte, node bool) []string {
	keys := []string{}
	content := normalizeDotEnvLineEndings(string(raw))
	lines := strings.FieldsFunc(content, func(r rune) bool {
		// JavaScript's multiline anchors also recognize these line terminators.
		return r == '\n' || node && (r == '\u2028' || r == '\u2029')
	})
	for _, line := range lines {
		line = strings.TrimFunc(line, dotEnvSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export") && len(line) > len("export") {
			r, _ := utf8.DecodeRuneInString(line[len("export"):])
			if dotEnvSpace(r) {
				line = strings.TrimLeftFunc(line[len("export"):], dotEnvSpace)
			}
		}
		key, tail := "", ""
		if strings.HasPrefix(line, "'") {
			// Also recognize Python-style quoted keys for the Node check. Node
			// ignores them; retaining a relevant name is a safe refusal.
			end := strings.IndexByte(line[1:], '\'')
			if end < 0 {
				// A malformed quoted header cannot be used as negative evidence.
				key = strings.TrimFunc(strings.SplitN(line[1:], "=", 2)[0], dotEnvSpace)
			} else {
				end++
				key, tail = line[1:end], line[end+1:]
			}
		} else {
			end := 0
			for i, r := range line {
				if node {
					if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
						break
					}
				} else if r == '=' || r == '#' || dotEnvSpace(r) {
					break
				}
				end = i + utf8.RuneLen(r)
			}
			key, tail = line[:end], line[end:]
		}
		if key == "" {
			continue
		}
		trimmed := strings.TrimLeftFunc(tail, dotEnvSpace)
		assigned := strings.HasPrefix(trimmed, "=")
		if node && strings.HasPrefix(tail, ":") {
			// A line break or trimmed trailing space after the colon is also
			// valid Node whitespace; keep a bare colon conservatively.
			r, _ := utf8.DecodeRuneInString(tail[1:])
			assigned = assigned || len(tail) == 1 || dotEnvSpace(r)
		}
		if assigned || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			keys = append(keys, key)
		}
	}
	return keys
}
