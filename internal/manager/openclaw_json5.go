package manager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	json5 "github.com/titanous/json5"
)

type openClawJSON5Kind uint8

const (
	openClawJSON5Scalar openClawJSON5Kind = iota
	openClawJSON5Object
	openClawJSON5Array
)

type openClawJSON5Node struct {
	kind        openClawJSON5Kind
	start, end  int
	open, close int
	fields      []*openClawJSON5Field
}

type openClawJSON5Field struct {
	key              string
	keyStart, keyEnd int
	value            *openClawJSON5Node
	comma            int
}

type openClawJSON5Document struct {
	raw  []byte
	root *openClawJSON5Node
}

type openClawJSON5Parser struct {
	raw []byte
	pos int
}

func parseOpenClawJSON5(raw []byte) (*openClawJSON5Document, error) {
	validated := raw
	if bytes.HasPrefix(validated, []byte{0xef, 0xbb, 0xbf}) {
		validated = validated[3:]
	}
	var checked json5.RawMessage
	if err := json5.Unmarshal(validated, &checked); err != nil {
		return nil, fmt.Errorf("OpenClaw JSON5 无效：%w", err)
	}

	p := &openClawJSON5Parser{raw: raw}
	p.skipTrivia()
	root, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipTrivia()
	if p.pos != len(raw) {
		return nil, fmt.Errorf("OpenClaw JSON5 在字节 %d 后含多余内容", p.pos)
	}
	return &openClawJSON5Document{raw: raw, root: root}, nil
}

func decodeOpenClawJSON5Value(raw []byte, value any) error {
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		raw = raw[3:]
	}
	return json5.Unmarshal(raw, value)
}

func decodeOpenClawJSON5RawObject(raw []byte) (map[string]json.RawMessage, error) {
	doc, err := parseOpenClawJSON5(raw)
	if err != nil {
		return nil, err
	}
	if doc.root.kind != openClawJSON5Object {
		return nil, nil
	}
	object := make(map[string]json.RawMessage, len(doc.root.fields))
	for _, field := range doc.root.fields {
		object[field.key] = cloneRawMessage(doc.raw[field.value.start:field.value.end])
	}
	return object, nil
}

func (p *openClawJSON5Parser) parseValue() (*openClawJSON5Node, error) {
	p.skipTrivia()
	if p.pos >= len(p.raw) {
		return nil, fmt.Errorf("OpenClaw JSON5 值意外结束")
	}
	switch p.raw[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '\'', '"':
		start := p.pos
		if err := p.scanString(); err != nil {
			return nil, err
		}
		return &openClawJSON5Node{kind: openClawJSON5Scalar, start: start, end: p.pos}, nil
	default:
		start := p.pos
		for p.pos < len(p.raw) {
			if isOpenClawJSON5ValueDelimiter(p.raw[p.pos]) || p.startsComment() || p.atWhitespace() {
				break
			}
			_, size := utf8.DecodeRune(p.raw[p.pos:])
			if size == 0 {
				break
			}
			p.pos += size
		}
		if p.pos == start {
			return nil, fmt.Errorf("OpenClaw JSON5 在字节 %d 缺少值", start)
		}
		return &openClawJSON5Node{kind: openClawJSON5Scalar, start: start, end: p.pos}, nil
	}
}

func (p *openClawJSON5Parser) parseObject() (*openClawJSON5Node, error) {
	node := &openClawJSON5Node{kind: openClawJSON5Object, start: p.pos, open: p.pos}
	p.pos++
	for {
		p.skipTrivia()
		if p.pos >= len(p.raw) {
			return nil, fmt.Errorf("OpenClaw JSON5 对象意外结束")
		}
		if p.raw[p.pos] == '}' {
			node.close = p.pos
			p.pos++
			node.end = p.pos
			return node, nil
		}

		field := &openClawJSON5Field{keyStart: p.pos, comma: -1}
		if p.raw[p.pos] == '\'' || p.raw[p.pos] == '"' {
			if err := p.scanString(); err != nil {
				return nil, err
			}
			field.keyEnd = p.pos
			if err := decodeOpenClawJSON5Value(p.raw[field.keyStart:field.keyEnd], &field.key); err != nil {
				return nil, fmt.Errorf("OpenClaw JSON5 对象键无效：%w", err)
			}
		} else {
			for p.pos < len(p.raw) && p.raw[p.pos] != ':' && !p.startsComment() && !p.atWhitespace() {
				_, size := utf8.DecodeRune(p.raw[p.pos:])
				if size == 0 {
					break
				}
				p.pos += size
			}
			field.keyEnd = p.pos
			if field.keyEnd == field.keyStart {
				return nil, fmt.Errorf("OpenClaw JSON5 在字节 %d 缺少对象键", p.pos)
			}
			wrapped := make([]byte, 0, field.keyEnd-field.keyStart+8)
			wrapped = append(wrapped, '{')
			wrapped = append(wrapped, p.raw[field.keyStart:field.keyEnd]...)
			wrapped = append(wrapped, ':', '0', '}')
			var keyObject map[string]any
			if err := decodeOpenClawJSON5Value(wrapped, &keyObject); err != nil {
				return nil, fmt.Errorf("OpenClaw JSON5 对象键无效：%w", err)
			}
			for key := range keyObject {
				field.key = key
			}
		}

		p.skipTrivia()
		if p.pos >= len(p.raw) || p.raw[p.pos] != ':' {
			return nil, fmt.Errorf("OpenClaw JSON5 对象键 %q 后缺少冒号", field.key)
		}
		p.pos++
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		field.value = value
		node.fields = append(node.fields, field)

		p.skipTrivia()
		if p.pos >= len(p.raw) {
			return nil, fmt.Errorf("OpenClaw JSON5 对象意外结束")
		}
		if p.raw[p.pos] == ',' {
			field.comma = p.pos
			p.pos++
			continue
		}
		if p.raw[p.pos] != '}' {
			return nil, fmt.Errorf("OpenClaw JSON5 对象在字节 %d 缺少逗号", p.pos)
		}
	}
}

func (p *openClawJSON5Parser) parseArray() (*openClawJSON5Node, error) {
	node := &openClawJSON5Node{kind: openClawJSON5Array, start: p.pos, open: p.pos}
	p.pos++
	for {
		p.skipTrivia()
		if p.pos >= len(p.raw) {
			return nil, fmt.Errorf("OpenClaw JSON5 数组意外结束")
		}
		if p.raw[p.pos] == ']' {
			node.close = p.pos
			p.pos++
			node.end = p.pos
			return node, nil
		}
		if _, err := p.parseValue(); err != nil {
			return nil, err
		}
		p.skipTrivia()
		if p.pos >= len(p.raw) {
			return nil, fmt.Errorf("OpenClaw JSON5 数组意外结束")
		}
		if p.raw[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.raw[p.pos] != ']' {
			return nil, fmt.Errorf("OpenClaw JSON5 数组在字节 %d 缺少逗号", p.pos)
		}
	}
}

func (p *openClawJSON5Parser) scanString() error {
	quote := p.raw[p.pos]
	p.pos++
	for p.pos < len(p.raw) {
		switch p.raw[p.pos] {
		case quote:
			p.pos++
			return nil
		case '\\':
			p.pos++
			if p.pos >= len(p.raw) {
				return fmt.Errorf("OpenClaw JSON5 字符串转义意外结束")
			}
			if p.raw[p.pos] == '\r' && p.pos+1 < len(p.raw) && p.raw[p.pos+1] == '\n' {
				p.pos += 2
			} else {
				_, size := utf8.DecodeRune(p.raw[p.pos:])
				p.pos += size
			}
		default:
			_, size := utf8.DecodeRune(p.raw[p.pos:])
			p.pos += size
		}
	}
	return fmt.Errorf("OpenClaw JSON5 字符串意外结束")
}

func (p *openClawJSON5Parser) skipTrivia() {
	for p.pos < len(p.raw) {
		if bytes.HasPrefix(p.raw[p.pos:], []byte{0xef, 0xbb, 0xbf}) {
			p.pos += 3
			continue
		}
		if p.atWhitespace() {
			_, size := utf8.DecodeRune(p.raw[p.pos:])
			p.pos += size
			continue
		}
		if p.pos+1 >= len(p.raw) || p.raw[p.pos] != '/' {
			return
		}
		switch p.raw[p.pos+1] {
		case '/':
			p.pos += 2
			for p.pos < len(p.raw) && p.raw[p.pos] != '\n' && p.raw[p.pos] != '\r' {
				p.pos++
			}
		case '*':
			p.pos += 2
			for p.pos+1 < len(p.raw) && !(p.raw[p.pos] == '*' && p.raw[p.pos+1] == '/') {
				p.pos++
			}
			if p.pos+1 < len(p.raw) {
				p.pos += 2
			}
		default:
			return
		}
	}
}

func (p *openClawJSON5Parser) startsComment() bool {
	return p.pos+1 < len(p.raw) && p.raw[p.pos] == '/' && (p.raw[p.pos+1] == '/' || p.raw[p.pos+1] == '*')
}

func (p *openClawJSON5Parser) atWhitespace() bool {
	if p.pos >= len(p.raw) {
		return false
	}
	r, _ := utf8.DecodeRune(p.raw[p.pos:])
	return unicode.IsSpace(r) || r == '\ufeff'
}

func isOpenClawJSON5ValueDelimiter(b byte) bool {
	return b == ',' || b == ']' || b == '}'
}

func openClawJSON5ObjectField(object *openClawJSON5Node, key, fieldName string) (*openClawJSON5Field, bool, error) {
	if object == nil || object.kind != openClawJSON5Object {
		return nil, false, fmt.Errorf("%s 必须是对象", fieldName)
	}
	var found *openClawJSON5Field
	count := 0
	for _, field := range object.fields {
		if field.key == key {
			found = field
			count++
		}
	}
	if count > 1 {
		return nil, false, fmt.Errorf("%s 重复定义，无法安全确定代理归属", fieldName)
	}
	return found, found != nil, nil
}

func openClawJSON5EffectiveFields(object *openClawJSON5Node) []*openClawJSON5Field {
	last := make(map[string]int, len(object.fields))
	for i, field := range object.fields {
		last[field.key] = i
	}
	fields := make([]*openClawJSON5Field, 0, len(last))
	for i, field := range object.fields {
		if last[field.key] == i {
			fields = append(fields, field)
		}
	}
	return fields
}

func canonicalOpenClawProxyValue(raw []byte) (json.RawMessage, error) {
	var value any
	if err := decodeOpenClawJSON5Value(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return json.RawMessage("null"), nil
	}
	proxy, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("必须是字符串或 null")
	}
	encoded, _ := json.Marshal(proxy)
	return encoded, nil
}

func openClawJSON5ProxyState(doc *openClawJSON5Document) (value json.RawMessage, present, channelsPresent, telegramPresent bool, err error) {
	if doc.root.kind != openClawJSON5Object {
		return nil, false, false, false, fmt.Errorf("OpenClaw 顶层配置必须是对象")
	}
	channelsField, channelsPresent, err := openClawJSON5ObjectField(doc.root, "channels", "channels")
	if err != nil || !channelsPresent {
		return nil, false, channelsPresent, false, err
	}
	if channelsField.value.kind != openClawJSON5Object {
		return nil, false, true, false, fmt.Errorf("channels 必须是对象")
	}
	telegramField, telegramPresent, err := openClawJSON5ObjectField(channelsField.value, "telegram", "channels.telegram")
	if err != nil || !telegramPresent {
		return nil, false, true, telegramPresent, err
	}
	if telegramField.value.kind != openClawJSON5Object {
		return nil, false, true, true, fmt.Errorf("channels.telegram 必须是对象")
	}
	proxyField, present, err := openClawJSON5ObjectField(telegramField.value, "proxy", "channels.telegram.proxy")
	if err != nil || !present {
		return nil, present, true, true, err
	}
	value, err = canonicalOpenClawProxyValue(doc.raw[proxyField.value.start:proxyField.value.end])
	if err != nil {
		return nil, false, true, true, fmt.Errorf("channels.telegram.proxy 无效：%w", err)
	}
	return value, true, true, true, nil
}

func openClawTelegramProxyLexeme(raw []byte) (string, bool, error) {
	doc, err := parseOpenClawJSON5(raw)
	if err != nil {
		return "", false, err
	}
	if _, present, _, _, err := openClawJSON5ProxyState(doc); err != nil || !present {
		return "", present, err
	}
	channels, _, _ := openClawJSON5ObjectField(doc.root, "channels", "channels")
	telegram, _, _ := openClawJSON5ObjectField(channels.value, "telegram", "channels.telegram")
	proxy, _, _ := openClawJSON5ObjectField(telegram.value, "proxy", "channels.telegram.proxy")
	return string(doc.raw[proxy.value.start:proxy.value.end]), true, nil
}

func setOpenClawJSON5Proxy(raw []byte, value json.RawMessage, present, structureRecorded, channelsPresent, telegramPresent bool) ([]byte, bool, error) {
	return setOpenClawJSON5ProxySource(raw, value, value, present, structureRecorded, channelsPresent, telegramPresent)
}

func setOpenClawJSON5ProxySource(raw []byte, value json.RawMessage, source []byte, present, structureRecorded, channelsPresent, telegramPresent bool) ([]byte, bool, error) {
	doc, err := parseOpenClawJSON5(raw)
	if err != nil {
		return nil, false, err
	}
	current, currentPresent, _, _, err := openClawJSON5ProxyState(doc)
	if err != nil {
		return nil, false, err
	}
	if proxyStatesEqual(current, currentPresent, value, present) {
		return raw, false, nil
	}
	if present {
		if err := validateOpenClawProxyRaw(value); err != nil {
			return nil, false, err
		}
		canonical, err := canonicalOpenClawProxyValue(source)
		if err != nil || !proxyStatesEqual(canonical, true, value, true) {
			return nil, false, fmt.Errorf("OpenClaw proxy 恢复词法值与 journal 原值不一致")
		}
		out, err := addOrReplaceOpenClawJSON5Proxy(doc, source)
		return out, err == nil, err
	}
	if !currentPresent {
		return raw, false, nil
	}
	out, err := removeOpenClawJSON5Proxy(doc, structureRecorded, channelsPresent, telegramPresent)
	return out, err == nil, err
}

func addOrReplaceOpenClawJSON5Proxy(doc *openClawJSON5Document, value json.RawMessage) ([]byte, error) {
	channels, channelsPresent, err := openClawJSON5ObjectField(doc.root, "channels", "channels")
	if err != nil {
		return nil, err
	}
	if !channelsPresent {
		nested := []byte(`{"telegram":{"proxy":` + string(value) + `}}`)
		return insertOpenClawJSON5Member(doc.raw, doc.root, "channels", nested)
	}
	if channels.value.kind != openClawJSON5Object {
		return nil, fmt.Errorf("channels 必须是对象")
	}
	telegram, telegramPresent, err := openClawJSON5ObjectField(channels.value, "telegram", "channels.telegram")
	if err != nil {
		return nil, err
	}
	if !telegramPresent {
		nested := []byte(`{"proxy":` + string(value) + `}`)
		return insertOpenClawJSON5Member(doc.raw, channels.value, "telegram", nested)
	}
	if telegram.value.kind != openClawJSON5Object {
		return nil, fmt.Errorf("channels.telegram 必须是对象")
	}
	proxy, proxyPresent, err := openClawJSON5ObjectField(telegram.value, "proxy", "channels.telegram.proxy")
	if err != nil {
		return nil, err
	}
	if !proxyPresent {
		return insertOpenClawJSON5Member(doc.raw, telegram.value, "proxy", value)
	}
	return replaceOpenClawJSON5Span(doc.raw, proxy.value.start, proxy.value.end, value), nil
}

func removeOpenClawJSON5Proxy(doc *openClawJSON5Document, structureRecorded, channelsPresent, telegramPresent bool) ([]byte, error) {
	channels, _, err := openClawJSON5ObjectField(doc.root, "channels", "channels")
	if err != nil || channels == nil || channels.value.kind != openClawJSON5Object {
		return nil, fmt.Errorf("无法定位 channels.telegram.proxy")
	}
	telegram, _, err := openClawJSON5ObjectField(channels.value, "telegram", "channels.telegram")
	if err != nil || telegram == nil || telegram.value.kind != openClawJSON5Object {
		return nil, fmt.Errorf("无法定位 channels.telegram.proxy")
	}
	proxy, _, err := openClawJSON5ObjectField(telegram.value, "proxy", "channels.telegram.proxy")
	if err != nil || proxy == nil {
		return nil, fmt.Errorf("无法定位 channels.telegram.proxy")
	}
	out := removeOpenClawJSON5Member(doc.raw, telegram.value, proxy)

	if !structureRecorded || telegramPresent {
		return out, nil
	}
	reparsed, err := parseOpenClawJSON5(out)
	if err != nil {
		return nil, err
	}
	channels, _, _ = openClawJSON5ObjectField(reparsed.root, "channels", "channels")
	telegram, _, _ = openClawJSON5ObjectField(channels.value, "telegram", "channels.telegram")
	if len(telegram.value.fields) != 0 || openClawJSON5ObjectContainsComment(reparsed.raw, telegram.value) {
		return out, nil
	}
	out = removeOpenClawJSON5Member(reparsed.raw, channels.value, telegram)
	if channelsPresent {
		return out, nil
	}
	reparsed, err = parseOpenClawJSON5(out)
	if err != nil {
		return nil, err
	}
	channels, _, _ = openClawJSON5ObjectField(reparsed.root, "channels", "channels")
	if len(channels.value.fields) != 0 || openClawJSON5ObjectContainsComment(reparsed.raw, channels.value) {
		return out, nil
	}
	return removeOpenClawJSON5Member(reparsed.raw, reparsed.root, channels), nil
}

func insertOpenClawJSON5Member(raw []byte, object *openClawJSON5Node, key string, value []byte) ([]byte, error) {
	encodedKey, _ := json.Marshal(key)
	member := append(append(append([]byte(nil), encodedKey...), ':', ' '), value...)
	newline := []byte("\n")
	if bytes.Contains(raw[object.open:object.close], []byte("\r\n")) {
		newline = []byte("\r\n")
	}
	lineStart, closeIndent, closeOnOwnLine := openClawJSON5ClosingIndent(raw, object.close)
	multiline := bytes.ContainsAny(raw[object.open:object.close], "\r\n")
	if multiline && closeOnOwnLine {
		indent := append([]byte(nil), closeIndent...)
		if len(object.fields) != 0 {
			if existing, ok := openClawJSON5LineIndent(raw, object.fields[len(object.fields)-1].keyStart); ok {
				indent = existing
			} else {
				indent = append(indent, ' ', ' ')
			}
		} else {
			indent = append(indent, ' ', ' ')
		}
		trailingComma := len(object.fields) != 0 && object.fields[len(object.fields)-1].comma >= 0
		insertion := append(append(append([]byte(nil), indent...), member...), newline...)
		if trailingComma {
			insertion = append(append(append([]byte(nil), indent...), member...), ',')
			insertion = append(insertion, newline...)
		}
		out := replaceOpenClawJSON5Span(raw, lineStart, lineStart, insertion)
		if len(object.fields) != 0 && !trailingComma {
			last := object.fields[len(object.fields)-1]
			out = replaceOpenClawJSON5Span(out, last.value.end, last.value.end, []byte{','})
		}
		return out, nil
	}

	if len(object.fields) == 0 {
		prefix := []byte(nil)
		if object.close > object.open+1 && !unicode.IsSpace(rune(raw[object.close-1])) {
			prefix = []byte{' '}
		}
		return replaceOpenClawJSON5Span(raw, object.close, object.close, append(prefix, member...)), nil
	}
	last := object.fields[len(object.fields)-1]
	if last.comma >= 0 {
		insertion := append([]byte{' '}, member...)
		insertion = append(insertion, ',')
		return replaceOpenClawJSON5Span(raw, object.close, object.close, insertion), nil
	}
	insertion := append([]byte{',', ' '}, member...)
	return replaceOpenClawJSON5Span(raw, object.close, object.close, insertion), nil
}

func removeOpenClawJSON5Member(raw []byte, object *openClawJSON5Node, target *openClawJSON5Field) []byte {
	index := -1
	for i, field := range object.fields {
		if field == target {
			index = i
			break
		}
	}
	if index < 0 {
		return raw
	}
	tokenEnd := target.value.end
	if target.comma >= 0 {
		tokenEnd = target.comma + 1
	}
	if lineStart, lineEnd, ok := openClawJSON5OwnLineSpan(raw, target.keyStart, tokenEnd); ok {
		out := replaceOpenClawJSON5Span(raw, lineStart, lineEnd, nil)
		if target.comma < 0 && index > 0 && object.fields[index-1].comma >= 0 {
			previousComma := object.fields[index-1].comma
			out = replaceOpenClawJSON5Span(out, previousComma, previousComma+1, nil)
		}
		return out
	}
	if target.comma < 0 && index > 0 && object.fields[index-1].comma >= 0 {
		previousComma := object.fields[index-1].comma
		between := raw[previousComma+1 : target.keyStart]
		if len(bytes.TrimSpace(between)) == 0 {
			return replaceOpenClawJSON5Span(raw, previousComma, target.value.end, nil)
		}
	}
	if target.comma >= 0 && index > 0 && object.fields[index-1].comma >= 0 {
		previousComma := object.fields[index-1].comma
		between := raw[previousComma+1 : target.keyStart]
		if len(bytes.TrimSpace(between)) == 0 {
			return replaceOpenClawJSON5Span(raw, previousComma+1, target.comma+1, nil)
		}
	}
	out := replaceOpenClawJSON5Span(raw, target.keyStart, target.value.end, nil)
	if target.comma >= 0 {
		adjusted := target.comma - (target.value.end - target.keyStart)
		return replaceOpenClawJSON5Span(out, adjusted, adjusted+1, nil)
	}
	if index > 0 && object.fields[index-1].comma >= 0 {
		previousComma := object.fields[index-1].comma
		return replaceOpenClawJSON5Span(out, previousComma, previousComma+1, nil)
	}
	return out
}

func openClawJSON5OwnLineSpan(raw []byte, start, end int) (int, int, bool) {
	lineStart := bytes.LastIndexAny(raw[:start], "\r\n") + 1
	if len(bytes.TrimSpace(raw[lineStart:start])) != 0 {
		return 0, 0, false
	}
	lineEnd := end
	for lineEnd < len(raw) && raw[lineEnd] != '\r' && raw[lineEnd] != '\n' {
		if raw[lineEnd] != ' ' && raw[lineEnd] != '\t' {
			return 0, 0, false
		}
		lineEnd++
	}
	if lineEnd < len(raw) && raw[lineEnd] == '\r' {
		lineEnd++
		if lineEnd < len(raw) && raw[lineEnd] == '\n' {
			lineEnd++
		}
	} else if lineEnd < len(raw) && raw[lineEnd] == '\n' {
		lineEnd++
	}
	return lineStart, lineEnd, true
}

func replaceOpenClawJSON5Span(raw []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(raw)-(end-start)+len(replacement))
	out = append(out, raw[:start]...)
	out = append(out, replacement...)
	out = append(out, raw[end:]...)
	return out
}

func openClawJSON5ClosingIndent(raw []byte, close int) (int, []byte, bool) {
	lineStart := bytes.LastIndexAny(raw[:close], "\r\n") + 1
	indent := raw[lineStart:close]
	for _, b := range indent {
		if b != ' ' && b != '\t' {
			return close, nil, false
		}
	}
	return lineStart, append([]byte(nil), indent...), true
}

func openClawJSON5LineIndent(raw []byte, position int) ([]byte, bool) {
	lineStart := bytes.LastIndexAny(raw[:position], "\r\n") + 1
	indent := raw[lineStart:position]
	for _, b := range indent {
		if b != ' ' && b != '\t' {
			return nil, false
		}
	}
	return append([]byte(nil), indent...), true
}

func openClawJSON5ObjectContainsComment(raw []byte, object *openClawJSON5Node) bool {
	inner := string(raw[object.open+1 : object.close])
	return strings.Contains(inner, "//") || strings.Contains(inner, "/*")
}
