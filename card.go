package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// This file turns Feishu interactive-card JSON (the "original card JSON"
// returned when card_msg_content_type=user_card_content is requested) into a
// small block tree the UI can draw: header, markdown / div text, hr, note,
// images, buttons, column_set, collapsible_panel, table ...
//
// Both card schema 1.0 ({"header":..,"elements":[..]}) and 2.0
// ({"schema":"2.0","header":..,"body":{"elements":[..]}}) are understood.

type jm = map[string]any

func asMap(v any) jm {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func strOf(m jm, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

func boolOf(m jm, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// textOf extracts a human string from a card text object:
// "x" | {"tag":"plain_text","content":"x"} | {"content":"x","i18n":{"zh_cn":"y"}}.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if i := asMap(t["i18n"]); i != nil {
			for _, l := range []string{"zh_cn", "zh_hk", "zh_tw", "en_us", "ja_jp"} {
				if s, ok := i[l].(string); ok && s != "" {
					return s
				}
			}
		}
		if s, ok := t["content"].(string); ok {
			return s
		}
		if s, ok := t["text"].(string); ok {
			return s
		}
		if s, ok := t["text"].(map[string]any); ok {
			return textOf(s)
		}
	}
	return ""
}

// isLarkMD reports whether a text object is lark_md (vs plain_text).
func isLarkMD(v any) bool {
	m := asMap(v)
	return m != nil && strOf(m, "tag") == "lark_md"
}

// ---------------- markdown (lark_md) inline parser ----------------

var (
	mdCode   = regexp.MustCompile("`([^`\n]+)`")
	mdBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdStrike = regexp.MustCompile(`~~(.+?)~~`)
	mdItalic = regexp.MustCompile(`\*([^*\n]+?)\*`)
	mdLink   = regexp.MustCompile(`\[([^\]\n]*)\]\(\s*([^)\s]+)\s*\)`)
	mdAt     = regexp.MustCompile(`(?i)<at\s+(?:id|user_id|email)\s*=\s*["']?([^"'>\s]*)["']?\s*>([^<]*)</at>`)
	mdPerson = regexp.MustCompile(`(?i)<person\s+[^>]*?id\s*=\s*["']?([^"'>\s]*)["']?[^>]*>\s*</person>`)
	mdA      = regexp.MustCompile(`(?is)<a\s+[^>]*?href\s*=\s*["']([^"']+)["'][^>]*>(.*?)</a>`)
	mdFont   = regexp.MustCompile(`(?is)<font\s+color\s*=\s*["']?([a-zA-Z]+)["']?\s*>(.*?)</font>`)
	mdTag    = regexp.MustCompile(`(?is)<text_tag[^>]*>(.*?)</text_tag>`)
	mdURL    = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+`)
	mdEmoji  = regexp.MustCompile(`:([A-Za-z][A-Za-z_]{1,24}):`)
	mdLocal  = regexp.MustCompile(`(?is)<(?:local_datetime|number_tag)[^>]*>(.*?)</(?:local_datetime|number_tag)>`)
	mdUserAt = regexp.MustCompile(`@_user_\d+|@_all`)
)

type mdRule struct {
	re   *regexp.Regexp
	kind string
}

var mdRules = []mdRule{
	{mdCode, "code"}, {mdBold, "bold"}, {mdStrike, "strike"}, {mdItalic, "italic"},
	{mdLink, "link"}, {mdAt, "at"}, {mdPerson, "person"}, {mdA, "a"}, {mdFont, "font"},
	{mdTag, "tag"}, {mdLocal, "plain"}, {mdUserAt, "user_at"}, {mdURL, "url"}, {mdEmoji, "emoji"},
}

func withStyle(st []string, add ...string) []string {
	out := make([]string, 0, len(st)+len(add))
	out = append(out, st...)
	return append(out, add...)
}

func isAllowedHref(h string) bool {
	l := strings.ToLower(h)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "mailto:")
}

// mdInline parses one line of lark_md into inline nodes.
func (b *builder) mdInline(s string, style []string) []Node {
	if s == "" {
		return nil
	}
	// find the earliest match (ties resolved by rule order)
	bestStart, bestEnd := -1, -1
	var best mdRule
	var bestM []int
	for _, r := range mdRules {
		loc := r.re.FindStringSubmatchIndex(s)
		if loc == nil {
			continue
		}
		if r.kind == "emoji" {
			name := s[loc[2]:loc[3]]
			if _, ok := emojiMap[name]; !ok {
				if _, ok := emojiMap[strings.ToUpper(name)]; !ok {
					continue
				}
			}
		}
		if bestStart < 0 || loc[0] < bestStart {
			bestStart, bestEnd, best, bestM = loc[0], loc[1], r, loc
		}
	}
	if bestStart < 0 {
		return []Node{{Type: "text", Text: s, Style: style}}
	}
	var out []Node
	if bestStart > 0 {
		out = append(out, Node{Type: "text", Text: s[:bestStart], Style: style})
	}
	g := func(i int) string {
		if bestM[2*i] < 0 {
			return ""
		}
		return s[bestM[2*i]:bestM[2*i+1]]
	}
	switch best.kind {
	case "code":
		out = append(out, Node{Type: "text", Text: g(1), Style: withStyle(style, "code")})
	case "bold":
		out = append(out, b.mdInline(g(1), withStyle(style, "bold"))...)
	case "strike":
		out = append(out, b.mdInline(g(1), withStyle(style, "lineThrough"))...)
	case "italic":
		out = append(out, b.mdInline(g(1), withStyle(style, "italic"))...)
	case "link", "a":
		text, href := g(1), g(2)
		if best.kind == "a" {
			text, href = g(2), g(1)
		}
		if isAllowedHref(href) {
			if strings.TrimSpace(text) == "" {
				text = href
			}
			n := Node{Type: "link", Text: stripTags(text), Href: href, Style: style, IsDoc: isDocURL(href)}
			out = append(out, n)
			b.addLink(href, n.Text)
		} else {
			out = append(out, Node{Type: "text", Text: stripTags(text), Style: style})
		}
	case "url":
		u := g(0)
		trimmed := strings.TrimRight(u, ".,;:!?'")
		for strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, "(") < strings.Count(trimmed, ")") {
			trimmed = strings.TrimSuffix(trimmed, ")")
		}
		bestEnd = bestStart + len(trimmed)
		out = append(out, Node{Type: "link", Text: trimmed, Href: trimmed, Style: style, IsDoc: isDocURL(trimmed)})
		b.addLink(trimmed, "")
	case "user_at":
		n := b.mi.atNode(g(0), "")
		n.Style = style
		out = append(out, n)
	case "at", "person":
		ref, hint := g(1), strings.TrimSpace(g(2))
		hint = strings.TrimPrefix(hint, "@")
		if hint == "" && b.opts.UserName != nil {
			if _, ok := b.mi.byKey[ref]; !ok {
				if _, ok := b.mi.byID[ref]; !ok {
					hint = b.opts.UserName(ref)
				}
			}
		}
		n := b.mi.atNode(ref, hint)
		if ref == "" {
			n = Node{Type: "at", Text: "@" + firstNonEmpty(hint, "用户")}
		}
		n.Style = style
		out = append(out, n)
	case "font":
		out = append(out, b.mdInline(g(2), withStyle(style, "c-"+strings.ToLower(g(1))))...)
	case "tag":
		out = append(out, b.mdInline(g(1), withStyle(style, "tag"))...)
	case "plain":
		out = append(out, b.mdInline(g(1), style)...)
	case "emoji":
		out = append(out, Node{Type: "emoji", Text: emojiFor(g(1))})
	}
	if bestEnd < len(s) {
		out = append(out, b.mdInline(s[bestEnd:], style)...)
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

var mdStrip = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")

var tagStripRe = regexp.MustCompile(`</?[A-Za-z][^>]*>`)

func stripTags(s string) string { return tagStripRe.ReplaceAllString(s, "") }

var (
	mdHeadingRe  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdHrRe       = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)
	mdListRe     = regexp.MustCompile(`^(\s*)(?:[-*+]|•)\s+(.*)$`)
	mdOrdListRe  = regexp.MustCompile(`^(\s*)(\d+[.)])\s+(.*)$`)
	mdQuoteRe    = regexp.MustCompile(`^>\s?(.*)$`)
	mdTableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(?:\|\s*:?-{2,}:?\s*)+\|?\s*$`)
	mdBrTagRe    = regexp.MustCompile(`(?i)<br\s*/?>`)
)

func listIndentLevel(ws string) int {
	spaces := len(strings.ReplaceAll(ws, "\t", "    "))
	switch {
	case spaces <= 0:
		return 0
	case spaces <= 4:
		return 1
	case spaces <= 8:
		return 2
	default:
		return 3
	}
}

func splitMDTableRow(ln string) []string {
	s := strings.TrimSpace(ln)
	if strings.HasPrefix(s, "|") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "|") && !strings.HasSuffix(s, `\|`) {
		s = s[:len(s)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if s[i] == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(s[i])
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

func isMDTableRow(ln string) bool {
	s := strings.TrimSpace(ln)
	if s == "" || !strings.Contains(s, "|") {
		return false
	}
	return len(splitMDTableRow(s)) >= 2
}

// mdParas converts lark_md / markdown into paragraphs of nodes.
func (b *builder) mdParas(s string) [][]Node {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	var out [][]Node
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			lang := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "```")))
			var code []string
			for i+1 < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "```") {
				i++
				code = append(code, lines[i])
			}
			i++ // closing fence
			out = append(out, []Node{{Type: "code", Text: strings.Join(code, "\n"), Lang: lang}})
			continue
		}
		if strings.TrimSpace(ln) == "" {
			if len(out) > 0 && len(out[len(out)-1]) > 0 {
				out = append(out, []Node{})
			}
			continue
		}
		if mdHrRe.MatchString(ln) {
			out = append(out, []Node{{Type: "hr"}})
			continue
		}
		// GFM Markdown table: header row + separator row + 0..N body rows
		if i+1 < len(lines) && isMDTableRow(ln) && mdTableSepRe.MatchString(lines[i+1]) {
			hdrRaw := splitMDTableRow(ln)
			numCols := len(hdrRaw)
			var tableCells [][][]Node
			parseRow := func(rawCells []string) [][]Node {
				row := make([][]Node, numCols)
				for c := 0; c < numCols; c++ {
					var cellTxt string
					if c < len(rawCells) {
						cellTxt = mdBrTagRe.ReplaceAllString(rawCells[c], "\n")
					}
					row[c] = b.mdInline(cellTxt, nil)
				}
				return row
			}
			tableCells = append(tableCells, parseRow(hdrRaw))
			i += 2 // advance past header and separator
			for i < len(lines) && isMDTableRow(lines[i]) && !mdTableSepRe.MatchString(lines[i]) {
				tableCells = append(tableCells, parseRow(splitMDTableRow(lines[i])))
				i++
			}
			i-- // outer loop will i++
			out = append(out, []Node{{Type: "table", Cells: tableCells}})
			continue
		}
		var style []string
		var block, marker string
		var indent int
		trimmed := strings.TrimSpace(ln)
		isStandaloneBold := strings.HasPrefix(trimmed, "**") && strings.HasSuffix(trimmed, "**") && len(trimmed) > 4 && !strings.Contains(trimmed[2:len(trimmed)-2], "**")
		if m := mdHeadingRe.FindStringSubmatch(ln); m != nil {
			ln, style = m[2], []string{"bold"}
			block = fmt.Sprintf("h%d", len(m[1]))
		} else if m := mdListRe.FindStringSubmatch(ln); m != nil {
			ln = m[1] + "• " + m[2]
			block, indent, marker = "ul", listIndentLevel(m[1]), "•"
		} else if m := mdOrdListRe.FindStringSubmatch(ln); m != nil {
			ln = m[1] + m[2] + " " + m[3]
			block, indent, marker = "ol", listIndentLevel(m[1]), m[2]
		} else if m := mdQuoteRe.FindStringSubmatch(ln); m != nil {
			ln, style = m[1], []string{"muted"}
			block = "quote"
		} else if isStandaloneBold {
			block = "sec"
		}
		if nodes := b.mdInline(ln, style); len(nodes) > 0 {
			if block != "" {
				nodes[0].Block = block
				nodes[0].Indent = indent
				nodes[0].Marker = marker
			}
			out = append(out, nodes)
		}
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// plainParas converts a plain_text value (no markdown, but links are linkified).
func (b *builder) plainParas(s string) [][]Node {
	var out [][]Node
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			if len(out) > 0 && len(out[len(out)-1]) > 0 {
				out = append(out, []Node{})
			}
			continue
		}
		nodes := tokenize(ln, b.mi)
		for _, n := range nodes {
			if n.Type == "link" {
				b.addLink(n.Href, "")
			}
		}
		out = append(out, nodes)
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// textParas renders a card text object according to its tag.
func (b *builder) textParas(v any) [][]Node {
	s := textOf(v)
	if s == "" {
		return nil
	}
	if isLarkMD(v) {
		return b.mdParas(s)
	}
	return b.plainParas(s)
}

// ---------------- card parsing ----------------

// isRawCard reports whether content looks like original card JSON (as opposed
// to the simplified {"title","elements":[[...]]} structure returned by default).
func isRawCard(m jm) bool {
	for _, k := range []string{"header", "body", "schema", "i18n_elements", "i18n_header", "card_link", "config"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	if els := asList(m["elements"]); len(els) > 0 {
		if _, ok := els[0].(map[string]any); ok {
			return true
		}
	}
	return false
}

func pickI18n(m jm, key string) any {
	if i := asMap(m["i18n_"+key]); i != nil {
		for _, l := range []string{"zh_cn", "zh_hk", "zh_tw", "en_us", "ja_jp"} {
			if v, ok := i[l]; ok {
				return v
			}
		}
	}
	return m[key]
}

// parseRawCard builds a Card from original card JSON. ok=false when the JSON
// has no recognisable card content.
func (b *builder) parseRawCard(m jm) (*Card, bool) {
	card := &Card{Blocks: []CardBlock{}}
	if h := asMap(pickI18n(m, "header")); h != nil {
		card.Title = textOf(h["title"])
		card.Subtitle = textOf(h["subtitle"])
		card.Template = strings.ToLower(strOf(h, "template"))
		for _, t := range asList(h["text_tag_list"]) {
			if s := textOf(asMap(t)["text"]); s != "" {
				card.Tags = append(card.Tags, s)
			}
		}
	}
	els := asList(pickI18n(m, "elements"))
	if body := asMap(m["body"]); body != nil {
		els = asList(body["elements"])
	}
	card.Blocks = b.cardElements(els, 0)
	if card.Title == "" && len(card.Blocks) == 0 {
		return nil, false
	}
	return card, true
}

const maxCardDepth = 6

func (b *builder) textBlock(paras [][]Node) (CardBlock, bool) {
	if len(paras) == 0 {
		return CardBlock{}, false
	}
	return CardBlock{Type: "text", Paras: paras}, true
}

func (b *builder) cardElements(els []any, depth int) []CardBlock {
	var out []CardBlock
	if depth > maxCardDepth {
		return out
	}
	for _, e := range els {
		m := asMap(e)
		if m == nil {
			continue
		}
		out = append(out, b.cardElement(m, depth)...)
	}
	return out
}

func (b *builder) buttonOf(m jm) CardButton {
	btn := CardButton{Text: textOf(m["text"]), Kind: strOf(m, "type")}
	if btn.Text == "" {
		btn.Text = textOf(m["placeholder"])
	}
	url := strOf(m, "url")
	if url == "" {
		if mu := asMap(m["multi_url"]); mu != nil {
			url = firstNonEmpty(strOf(mu, "url"), strOf(mu, "pc_url"))
		}
	}
	if url == "" {
		for _, bh := range asList(m["behaviors"]) {
			bm := asMap(bh)
			if strOf(bm, "type") == "open_url" {
				url = firstNonEmpty(strOf(bm, "default_url"), strOf(bm, "pc_url"))
				break
			}
		}
	}
	if isAllowedHref(url) {
		btn.URL = url
		b.addLink(url, btn.Text)
	}
	switch strOf(m, "tag") {
	case "button":
	default:
		btn.Kind = "select"
		if btn.Text == "" {
			btn.Text = "选择…"
		}
	}
	if btn.Text == "" {
		btn.Text = "按钮"
	}
	return btn
}

func cellText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strings.TrimSuffix(fmt.Sprintf("%v", t), ".0")
	case bool:
		return fmt.Sprint(t)
	case []any:
		var parts []string
		for _, x := range t {
			if s := cellText(x); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "、")
	case map[string]any:
		for _, k := range []string{"text", "content", "name"} {
			if s := cellText(t[k]); s != "" {
				return s
			}
		}
	}
	return ""
}

func (b *builder) cardElement(m jm, depth int) []CardBlock {
	tag := strOf(m, "tag")
	one := func(blk CardBlock, ok bool) []CardBlock {
		if !ok {
			return nil
		}
		return []CardBlock{blk}
	}
	switch tag {
	case "markdown":
		return one(b.textBlock(b.mdParas(firstNonEmpty(strOf(m, "content"), textOf(m["text"])))))
	case "div":
		var out []CardBlock
		if blk, ok := b.textBlock(b.textParas(m["text"])); ok {
			out = append(out, blk)
		}
		if fs := asList(m["fields"]); len(fs) > 0 {
			f := CardBlock{Type: "fields"}
			for _, x := range fs {
				fm := asMap(x)
				if blk, ok := b.textBlock(b.textParas(fm["text"])); ok {
					blk.Short = boolOf(fm, "is_short")
					f.Children = append(f.Children, blk)
				}
			}
			if len(f.Children) > 0 {
				out = append(out, f)
			}
		}
		if ex := asMap(m["extra"]); ex != nil {
			switch strOf(ex, "tag") {
			case "button", "select_static", "overflow", "date_picker":
				out = append(out, CardBlock{Type: "actions", Buttons: []CardButton{b.buttonOf(ex)}})
			case "img":
				out = append(out, b.cardImage(ex))
			}
		}
		return out
	case "hr":
		return []CardBlock{{Type: "hr"}}
	case "img":
		return []CardBlock{b.cardImage(m)}
	case "img_combination":
		var out []CardBlock
		for _, x := range asList(m["img_list"]) {
			out = append(out, b.cardImage(asMap(x)))
		}
		return out
	case "note":
		blk := CardBlock{Type: "note"}
		var row []Node
		for _, x := range asList(m["elements"]) {
			xm := asMap(x)
			switch strOf(xm, "tag") {
			case "img", "standard_icon", "custom_icon":
				if k := strOf(xm, "img_key"); keyRe.MatchString(k) {
					row = append(row, Node{Type: "image", Key: k, NoDL: true})
					b.addAttachment(Attachment{Kind: "image", Name: "图片", FileKey: k, ResType: "image", NoDownload: true})
				}
			default:
				for _, p := range b.textParas(xm) {
					if len(row) > 0 {
						row = append(row, Node{Type: "text", Text: " "})
					}
					row = append(row, p...)
				}
			}
		}
		if len(row) > 0 {
			blk.Paras = [][]Node{row}
			return []CardBlock{blk}
		}
		return nil
	case "action":
		var btns []CardButton
		for _, x := range asList(m["actions"]) {
			btns = append(btns, b.buttonOf(asMap(x)))
		}
		if len(btns) > 0 {
			return []CardBlock{{Type: "actions", Buttons: btns}}
		}
		return nil
	case "button", "select_static", "multi_select_static", "overflow", "date_picker", "picker_time", "picker_datetime", "select_person", "multi_select_person":
		return []CardBlock{{Type: "actions", Buttons: []CardButton{b.buttonOf(m)}}}
	case "column_set":
		blk := CardBlock{Type: "columns"}
		for _, c := range asList(m["columns"]) {
			cm := asMap(c)
			w := 1
			if f, ok := cm["weight"].(float64); ok && f >= 1 && f <= 5 {
				w = int(f)
			}
			if strOf(cm, "width") == "auto" {
				w = 0
			}
			blk.Cols = append(blk.Cols, b.cardElements(asList(cm["elements"]), depth+1))
			blk.Weights = append(blk.Weights, w)
		}
		if len(blk.Cols) > 0 {
			return []CardBlock{blk}
		}
		return nil
	case "collapsible_panel":
		blk := CardBlock{Type: "collapse", Open: boolOf(m, "expanded")}
		if h := asMap(m["header"]); h != nil {
			blk.Title = textOf(h["title"])
		}
		blk.Title = strings.TrimSpace(mdStrip.Replace(stripTags(firstNonEmpty(blk.Title, "详情"))))
		blk.Children = b.cardElements(asList(m["elements"]), depth+1)
		return []CardBlock{blk}
	case "interactive_container", "form", "repeat":
		if kids := b.cardElements(asList(m["elements"]), depth+1); len(kids) > 0 {
			return []CardBlock{{Type: "group", Children: kids}}
		}
		return nil
	case "table":
		blk := CardBlock{Type: "table"}
		var names []string
		for _, c := range asList(m["columns"]) {
			cm := asMap(c)
			names = append(names, strOf(cm, "name"))
			blk.Header = append(blk.Header, firstNonEmpty(textOf(cm["display_name"]), strOf(cm, "name")))
		}
		for _, r := range asList(m["rows"]) {
			rm := asMap(r)
			row := make([]string, len(names))
			for i, n := range names {
				row[i] = stripTags(cellText(rm[n]))
			}
			blk.Rows = append(blk.Rows, row)
		}
		if len(blk.Header) > 0 {
			return []CardBlock{blk}
		}
		return nil
	case "person":
		return one(b.textBlock([][]Node{{b.mi.atNode(strOf(m, "user_id"), b.userName(strOf(m, "user_id")))}}))
	case "person_list":
		var row []Node
		for _, p := range asList(m["persons"]) {
			if len(row) > 0 {
				row = append(row, Node{Type: "text", Text: " "})
			}
			row = append(row, b.mi.atNode(strOf(asMap(p), "id"), b.userName(strOf(asMap(p), "id"))))
		}
		if len(row) > 0 {
			return []CardBlock{{Type: "text", Paras: [][]Node{row}}}
		}
		return nil
	case "chart":
		return []CardBlock{{Type: "text", Paras: [][]Node{{{Type: "text", Text: "[图表]（请在飞书中查看）", Style: []string{"muted"}}}}}}
	case "input":
		return one(b.textBlock(b.textParas(m["label"])))
	case "checker":
		return one(b.textBlock(b.textParas(m["text"])))
	}
	// unknown component: show any text it carries, never a bare error
	if s := firstNonEmpty(strOf(m, "content"), textOf(m["text"])); s != "" {
		return one(b.textBlock(b.mdParas(s)))
	}
	if kids := asList(m["elements"]); len(kids) > 0 {
		return b.cardElements(kids, depth+1)
	}
	return []CardBlock{{Type: "text", Paras: [][]Node{{{Type: "text", Text: "[卡片组件: " + tag + "]", Style: []string{"muted"}}}}}}
}

func (b *builder) cardImage(m jm) CardBlock {
	k := strOf(m, "img_key")
	if !keyRe.MatchString(k) {
		return CardBlock{Type: "text", Paras: [][]Node{{{Type: "text", Text: "[图片]", Style: []string{"muted"}}}}}
	}
	alt := textOf(m["alt"])
	if t := textOf(m["title"]); t != "" {
		alt = t
	}
	b.addAttachment(Attachment{Kind: "image", Name: firstNonEmpty(alt, "图片"), FileKey: k, ResType: "image", NoDownload: true})
	return CardBlock{Type: "image", Key: k, Title: alt}
}

// cardPlain flattens a card into searchable plain text (title first).
func cardPlain(c *Card) string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, c.Title)
	}
	var walk func(bs []CardBlock)
	walk = func(bs []CardBlock) {
		for _, bl := range bs {
			for _, p := range bl.Paras {
				if s := strings.TrimSpace(plainOf(p)); s != "" {
					lines = append(lines, s)
				}
			}
			if bl.Type == "collapse" && bl.Title != "" {
				lines = append(lines, bl.Title)
			}
			for _, bt := range bl.Buttons {
				lines = append(lines, "["+bt.Text+"]")
			}
			for _, r := range bl.Rows {
				lines = append(lines, strings.Join(r, " "))
			}
			walk(bl.Children)
			for _, col := range bl.Cols {
				walk(col)
			}
		}
	}
	walk(c.Blocks)
	return strings.Join(lines, "\n")
}

// parseCardContent parses interactive-message content. It returns the Card
// when the content is original card JSON.
func (b *builder) parseCardContent(content string) (*Card, bool) {
	var m jm
	if err := json.Unmarshal([]byte(content), &m); err != nil || !isRawCard(m) {
		return nil, false
	}
	return b.parseRawCard(m)
}

func (b *builder) userName(id string) string {
	if b.opts.UserName == nil || id == "" {
		return ""
	}
	return b.opts.UserName(id)
}

var openIDRe = regexp.MustCompile(`ou_[A-Za-z0-9_]+`)

// cardUserIDs lists open_ids referenced inside card JSON (<at id=…>, person
// components) so their names can be resolved like message senders.
func cardUserIDs(content string) []string {
	return openIDRe.FindAllString(content, -1)
}
