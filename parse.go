package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	tokenRe = regexp.MustCompile(`\[[^\]\n]+\]\((?:https?://|mailto:)[^)\s]+\)|@_user_\d+|@_all|https?://[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+`)
	// Feishu image/file keys only contain these characters.
	keyRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,200}$`)
)

var docPathPrefixes = []string{"/doc/", "/docx/", "/docs/", "/wiki/", "/sheets/", "/base/", "/bitable/", "/file/", "/mindnotes/", "/slides/", "/minutes/", "/drive/"}

// isDocURL reports whether href looks like a Feishu/Lark document link.
func isDocURL(href string) bool {
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	ok := false
	for _, suf := range []string{"feishu.cn", "larksuite.com", "larkoffice.com", "feishu.net", "feishu-pre.net"} {
		if h == suf || strings.HasSuffix(h, "."+suf) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	for _, p := range docPathPrefixes {
		if strings.HasPrefix(u.Path, p) {
			return true
		}
	}
	return false
}

var emojiMap = map[string]string{
	"SMILE": "😊", "LAUGH": "😄", "JIAYI": "👌", "OK": "👌", "THUMBSUP": "👍", "THUMBSDOWN": "👎",
	"HEART": "❤️", "CLAP": "👏", "APPLAUSE": "👏", "MUSCLE": "💪", "FIRE": "🔥", "PARTY": "🎉",
	"TEARS": "😢", "CRY": "😭", "WINK": "😉", "BLUSH": "😊", "SMIRK": "😏", "SOB": "😭",
	"THINKING": "🤔", "SALUTE": "🫡", "YEAH": "✌️", "CHECK": "✅", "DONE": "✅", "ROSE": "🌹",
	"GLANCE": "👀", "PRAY": "🙏", "THANKS": "🙏", "FACEPALM": "🤦", "SHY": "😳", "AWESOME": "🤩",
	"WOW": "😮", "SWEAT": "😅", "LOL": "😂", "SMART": "🧠", "COOL": "😎", "KISS": "😘",
	"ERROR": "❌", "EMBARRASSED": "😅", "HUG": "🤗", "ENOUGH": "🙅", "OnIt": "🫡",
}

func emojiFor(t string) string {
	if e, ok := emojiMap[t]; ok {
		return e
	}
	if e, ok := emojiMap[strings.ToUpper(t)]; ok {
		return e
	}
	return "[" + t + "]"
}

// mentionIndex resolves @ placeholders / ids to display names.
type mentionIndex struct {
	byKey map[string]Mention
	byID  map[string]Mention
}

func newMentionIndex(ms []Mention) mentionIndex {
	mi := mentionIndex{byKey: map[string]Mention{}, byID: map[string]Mention{}}
	for _, m := range ms {
		if m.Key != "" {
			mi.byKey[m.Key] = m
		}
		if m.ID != "" {
			mi.byID[m.ID] = m
		}
	}
	return mi
}

// atNode builds an @ node from a key ("@_user_1"), an open_id or "all".
func (mi mentionIndex) atNode(ref, hintName string) Node {
	if ref == "all" || ref == "@_all" {
		return Node{Type: "at", Text: "@所有人", UserID: "all"}
	}
	if m, ok := mi.byKey[ref]; ok {
		name := m.Name
		if name == "" {
			name = hintName
		}
		if name == "" {
			name = "用户"
		}
		return Node{Type: "at", Text: "@" + name, UserID: m.ID}
	}
	if m, ok := mi.byID[ref]; ok && m.Name != "" {
		return Node{Type: "at", Text: "@" + m.Name, UserID: m.ID}
	}
	name := hintName
	if name == "" {
		name = "用户"
	}
	id := ref
	if strings.HasPrefix(ref, "@_user_") {
		id = ""
	}
	return Node{Type: "at", Text: "@" + name, UserID: id}
}

// tokenize splits a plain line into text / at / link nodes.
func tokenize(line string, mi mentionIndex) []Node {
	var nodes []Node
	last := 0
	for _, loc := range tokenRe.FindAllStringIndex(line, -1) {
		s, e := loc[0], loc[1]
		tok := line[s:e]
		if strings.HasPrefix(tok, "[") {
			// [text](url) hyperlink as used in text messages
			if i := strings.LastIndex(tok, "]("); i > 0 {
				text, href := tok[1:i], tok[i+2:len(tok)-1]
				if s > last {
					nodes = append(nodes, Node{Type: "text", Text: line[last:s]})
				}
				nodes = append(nodes, Node{Type: "link", Text: text, Href: href, IsDoc: isDocURL(href)})
				last = e
				continue
			}
		}
		if strings.HasPrefix(tok, "http") {
			// trim trailing punctuation that is rarely part of the URL
			trimmed := strings.TrimRight(tok, ".,;:!?'")
			for strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, "(") < strings.Count(trimmed, ")") {
				trimmed = strings.TrimSuffix(trimmed, ")")
			}
			e = s + len(trimmed)
			tok = trimmed
		}
		if s > last {
			nodes = append(nodes, Node{Type: "text", Text: line[last:s]})
		}
		if strings.HasPrefix(tok, "http") {
			nodes = append(nodes, Node{Type: "link", Text: tok, Href: tok, IsDoc: isDocURL(tok)})
		} else {
			nodes = append(nodes, mi.atNode(tok, ""))
		}
		last = e
	}
	if last < len(line) {
		nodes = append(nodes, Node{Type: "text", Text: line[last:]})
	}
	return nodes
}

type postNode struct {
	Tag       string   `json:"tag"`
	Text      string   `json:"text"`
	Href      string   `json:"href"`
	UserID    string   `json:"user_id"`
	UserName  string   `json:"user_name"`
	ImageKey  string   `json:"image_key"`
	FileKey   string   `json:"file_key"`
	EmojiType string   `json:"emoji_type"`
	Language  string   `json:"language"`
	Style     []string `json:"style"`
}

// builder accumulates paragraphs, plain text and attachments.
type builder struct {
	opts  ParseOpts
	mi    mentionIndex
	c     Content
	plain []string
	noDL  bool // images/files seen while parsing can never be downloaded
}

func (b *builder) addAttachment(a Attachment) { b.c.Attachments = append(b.c.Attachments, a) }

func (b *builder) addLink(href, name string) {
	kind := "link"
	if isDocURL(href) {
		kind = "doc"
	}
	if name == "" {
		name = href
	}
	b.addAttachment(Attachment{Kind: kind, Name: name, URL: href})
}

func plainOf(nodes []Node) string {
	var sb strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text", "at":
			sb.WriteString(n.Text)
		case "link":
			sb.WriteString(n.Text)
		case "image":
			sb.WriteString("[图片]")
		case "media":
			sb.WriteString("[视频]")
		case "file":
			sb.WriteString("[文件] " + n.Text)
		case "emoji":
			sb.WriteString(n.Text)
		case "code":
			sb.WriteString(n.Text)
		case "table":
			for ri, row := range n.Cells {
				if ri > 0 {
					sb.WriteString("\n")
				}
				for ci, cell := range row {
					if ci > 0 {
						sb.WriteString(" | ")
					}
					sb.WriteString(plainOf(cell))
				}
			}
		}
	}
	return sb.String()
}

func (b *builder) addParagraph(nodes []Node) {
	if len(nodes) == 0 {
		return
	}
	if nodes[0].Type == "text" && nodes[0].Block == "" {
		if m := mdListRe.FindStringSubmatch(nodes[0].Text); m != nil {
			nodes[0].Block, nodes[0].Indent, nodes[0].Marker = "ul", listIndentLevel(m[1]), "•"
		} else if m := mdOrdListRe.FindStringSubmatch(nodes[0].Text); m != nil {
			nodes[0].Block, nodes[0].Indent, nodes[0].Marker = "ol", listIndentLevel(m[1]), m[2]
		}
	}
	b.c.Paragraphs = append(b.c.Paragraphs, nodes)
	b.plain = append(b.plain, plainOf(nodes))
}

func (b *builder) finish() Content {
	c := b.c
	if c.Paragraphs == nil {
		c.Paragraphs = [][]Node{}
	}
	// trim trailing blank plain lines
	p := strings.TrimSpace(strings.Join(b.plain, "\n"))
	if c.Title != "" {
		p = strings.TrimSpace(c.Title + "\n" + p)
	}
	c.Plain = p
	return c
}

// cleanDocLink splits trailing non-ASCII text accidentally attached to a Feishu doc/file URL.
func cleanDocLink(href, text string) (cleanHref, cleanText, tail string) {
	cleanHref, cleanText = href, text
	if !isDocURL(href) {
		return
	}
	for i, r := range href {
		if r > 127 {
			cleanHref = href[:i]
			tail = href[i:]
			if text == href || text == "" {
				cleanText = cleanHref
			} else if strings.HasSuffix(text, tail) {
				cleanText = strings.TrimSuffix(text, tail)
			} else {
				tail = ""
			}
			return
		}
	}
	return
}

func (b *builder) fromPostNodes(rows [][]postNode) {
	for _, row := range rows {
		var nodes []Node
		for _, pn := range row {
			switch pn.Tag {
			case "text":
				nodes = append(nodes, Node{Type: "text", Text: pn.Text, Style: pn.Style})
			case "a":
				href, txt, tail := cleanDocLink(pn.Href, pn.Text)
				if txt == "" {
					txt = href
				}
				nodes = append(nodes, Node{Type: "link", Text: txt, Href: href, Style: pn.Style, IsDoc: isDocURL(href)})
				if href != "" {
					name := pn.Text
					if tail != "" && name == pn.Href {
						name = ""
					}
					b.addLink(href, name)
				}
				if tail != "" {
					nodes = append(nodes, Node{Type: "text", Text: tail, Style: pn.Style})
				}
			case "at":
				n := b.mi.atNode(pn.UserID, pn.UserName)
				n.Style = pn.Style
				nodes = append(nodes, n)
			case "img":
				if keyRe.MatchString(pn.ImageKey) {
					nodes = append(nodes, Node{Type: "image", Key: pn.ImageKey, NoDL: b.noDL})
					b.addAttachment(Attachment{Kind: "image", Name: "图片", FileKey: pn.ImageKey, ResType: "image", NoDownload: b.noDL})
				}
			case "media":
				if keyRe.MatchString(pn.FileKey) {
					nodes = append(nodes, Node{Type: "media", Key: pn.FileKey, Cover: pn.ImageKey, Text: "视频", NoDL: b.noDL})
					b.addAttachment(Attachment{Kind: "video", Name: "视频", FileKey: pn.FileKey, ResType: "file", CoverKey: pn.ImageKey, NoDownload: b.noDL})
				}
			case "emotion":
				nodes = append(nodes, Node{Type: "emoji", Text: emojiFor(pn.EmojiType)})
			case "hr":
				nodes = append(nodes, Node{Type: "hr"})
			case "md":
				if len(nodes) > 0 {
					b.addParagraph(nodes)
					nodes = nil
				}
				for _, p := range b.mdParas(pn.Text) {
					if len(p) == 0 {
						b.c.Paragraphs = append(b.c.Paragraphs, []Node{})
						b.plain = append(b.plain, "")
					} else {
						b.addParagraph(p)
					}
				}
			case "code_block":
				nodes = append(nodes, Node{Type: "code", Text: pn.Text, Lang: strings.ToLower(pn.Language)})
				// Private attachment links are sent as literal blocks to avoid IM
				// auto-authorization. Still discover them for the local file panel.
				literal := strings.TrimSpace(pn.Text)
				if parts := tokenize(literal, newMentionIndex(nil)); len(parts) == 1 && parts[0].Type == "link" && parts[0].Href == literal && parts[0].IsDoc {
					b.addLink(literal, "")
				}
			case "button":
				if pn.Text != "" {
					nodes = append(nodes, Node{Type: "text", Text: "[" + pn.Text + "]"})
				}
			default:
				if pn.Text != "" {
					nodes = append(nodes, Node{Type: "text", Text: pn.Text, Style: pn.Style})
				}
			}
		}
		b.addParagraph(nodes)
	}
}

func looksLikeMarkdown(s string) bool {
	if strings.Contains(s, "**") || strings.Contains(s, "```") || strings.Contains(s, "`") {
		return true
	}
	for _, ln := range strings.Split(s, "\n") {
		if mdHeadingRe.MatchString(ln) || mdListRe.MatchString(ln) || mdOrdListRe.MatchString(ln) || mdQuoteRe.MatchString(ln) || mdTableSepRe.MatchString(ln) {
			return true
		}
	}
	return false
}

func (b *builder) fromText(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if looksLikeMarkdown(s) {
		for _, p := range b.mdParas(s) {
			if len(p) == 0 {
				b.c.Paragraphs = append(b.c.Paragraphs, []Node{})
				b.plain = append(b.plain, "")
			} else {
				b.addParagraph(p)
			}
		}
		return
	}
	lines := strings.Split(s, "\n")
	// drop leading/trailing blank lines
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			b.c.Paragraphs = append(b.c.Paragraphs, []Node{})
			b.plain = append(b.plain, "")
			continue
		}
		nodes := tokenize(ln, b.mi)
		for _, n := range nodes {
			if n.Type == "link" {
				name := n.Text
				if name == n.Href {
					name = ""
				}
				b.addLink(n.Href, name)
			}
		}
		b.addParagraph(nodes)
	}
}

// ParseOpts supplies optional lookups used for readable summaries.
type ParseOpts struct {
	// AppLink is the AppLink host ("https://applink.feishu.cn").
	AppLink string
	// ChatName / UserName resolve ids from local caches ("" = unknown).
	ChatName func(chatID string) string
	UserName func(openID string) string
}

// cardPlaceholder is what Feishu puts in the body of messages it cannot
// express in the simplified card structure.
const cardPlaceholder = "请升级至最新版本客户端，以查看内容"

// isCardPlaceholder reports whether s contains Feishu's "please upgrade client" placeholder.
func isCardPlaceholder(s string) bool {
	return strings.Contains(s, cardPlaceholder) ||
		strings.Contains(s, "请升级至最新版本客户端") ||
		strings.Contains(s, "请将客户端升级至最新版本")
}

// ParseContent converts a Feishu message body (JSON string) into the render model.
func ParseContent(msgType, content string, mentions []Mention) Content {
	return ParseContentOpts(msgType, content, mentions, ParseOpts{})
}

func fmtMillis(v string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	return time.UnixMilli(n).Format("2006-01-02 15:04")
}

// ParseContentOpts is ParseContent with optional lookups.
func ParseContentOpts(msgType, content string, mentions []Mention, opts ParseOpts) Content {
	b := &builder{mi: newMentionIndex(mentions), opts: opts}
	placeholder := func(s string) Content {
		b.addParagraph([]Node{{Type: "text", Text: s, Style: []string{"muted"}}})
		return b.finish()
	}
	// summary renders "[kind] detail" with the kind in muted style.
	summary := func(kind string, detail ...string) Content {
		var nodes []Node
		nodes = append(nodes, Node{Type: "text", Text: "[" + kind + "]", Style: []string{"muted"}})
		if d := strings.TrimSpace(strings.Join(detail, " ")); d != "" {
			nodes = append(nodes, Node{Type: "text", Text: " " + d})
		}
		b.addParagraph(nodes)
		return b.finish()
	}
	switch msgType {
	case "text":
		var t struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &t); err != nil {
			// some clients deliver plain text; be lenient
			t.Text = content
		}
		b.fromText(t.Text)
	case "post":
		title, rows := parsePostJSON(content)
		b.c.Title = title
		b.fromPostNodes(rows)
	case "image":
		var t struct {
			ImageKey string `json:"image_key"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		if !keyRe.MatchString(t.ImageKey) {
			return placeholder("[图片]")
		}
		b.addParagraph([]Node{{Type: "image", Key: t.ImageKey}})
		b.addAttachment(Attachment{Kind: "image", Name: "图片", FileKey: t.ImageKey, ResType: "image"})
	case "file":
		var t struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		if !keyRe.MatchString(t.FileKey) {
			return placeholder("[文件]")
		}
		if t.FileName == "" {
			t.FileName = "文件"
		}
		b.addParagraph([]Node{{Type: "file", Key: t.FileKey, Text: t.FileName}})
		b.addAttachment(Attachment{Kind: "file", Name: t.FileName, FileKey: t.FileKey, ResType: "file"})
	case "audio":
		var t struct {
			FileKey  string `json:"file_key"`
			Duration int    `json:"duration"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		if !keyRe.MatchString(t.FileKey) {
			return placeholder("[语音]")
		}
		name := "语音消息"
		if t.Duration > 0 {
			name = fmt.Sprintf("语音消息 (%d秒)", (t.Duration+500)/1000)
		}
		b.addParagraph([]Node{{Type: "file", Key: t.FileKey, Text: name, Style: []string{"audio"}}})
		b.addAttachment(Attachment{Kind: "audio", Name: name, FileKey: t.FileKey, ResType: "file"})
	case "media":
		var t struct {
			FileKey  string `json:"file_key"`
			ImageKey string `json:"image_key"`
			FileName string `json:"file_name"`
			Duration int    `json:"duration"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		if !keyRe.MatchString(t.FileKey) {
			return placeholder("[视频]")
		}
		if t.FileName == "" {
			t.FileName = "视频"
		}
		cover := ""
		if keyRe.MatchString(t.ImageKey) {
			cover = t.ImageKey
		}
		b.addParagraph([]Node{{Type: "media", Key: t.FileKey, Cover: cover, Text: t.FileName}})
		b.addAttachment(Attachment{Kind: "video", Name: t.FileName, FileKey: t.FileKey, ResType: "file", CoverKey: cover})
	case "folder":
		var t struct {
			FileName string `json:"file_name"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		return summary("文件夹", t.FileName, "（接口不支持下载文件夹，请在飞书中查看）")
	case "sticker":
		return summary("表情包")
	case "share_chat":
		var t struct {
			ChatID string `json:"chat_id"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		name := ""
		if opts.ChatName != nil && t.ChatID != "" {
			name = opts.ChatName(t.ChatID)
		}
		b.addParagraph([]Node{{Type: "text", Text: "[群名片]", Style: []string{"muted"}}})
		if name != "" && name != t.ChatID {
			b.addParagraph([]Node{{Type: "text", Text: "👥 " + name, Style: []string{"bold"}}})
		}
		if opts.AppLink != "" && keyRe.MatchString(t.ChatID) {
			href := opts.AppLink + "/client/chat/open?openChatId=" + url.QueryEscape(t.ChatID)
			b.addParagraph([]Node{{Type: "link", Text: "在飞书中打开该群", Href: href}})
		}
		return b.finish()
	case "share_user":
		var t struct {
			UserID string `json:"user_id"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		name := ""
		if opts.UserName != nil && t.UserID != "" {
			name = opts.UserName(t.UserID)
		}
		if name == "" {
			if m, ok := b.mi.byID[t.UserID]; ok {
				name = m.Name
			}
		}
		if name == "" && t.UserID != "" {
			name = "用户 " + shortID(t.UserID)
		}
		return summary("个人名片", "👤 "+name)
	case "location":
		var t struct {
			Name      string `json:"name"`
			Longitude string `json:"longitude"`
			Latitude  string `json:"latitude"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		coords := ""
		if t.Latitude != "" && t.Longitude != "" {
			coords = fmt.Sprintf("（%s, %s）", t.Latitude, t.Longitude)
		}
		return summary("位置", "📍 "+t.Name, coords)
	case "merge_forward":
		// expanded by the service (sub-messages come from the get-message API);
		// this is only the fallback when that is not possible.
		return summary("合并转发的聊天记录")
	case "interactive":
		return parseInteractive(b, content)
	case "system":
		var t struct {
			Template    string         `json:"template"`
			DividerText map[string]any `json:"divider_text"`
		}
		var raw map[string]any
		_ = json.Unmarshal([]byte(content), &raw)
		_ = json.Unmarshal([]byte(content), &t)
		text := systemText(t.Template, raw)
		if text == "" {
			text = "系统消息"
		}
		b.addParagraph([]Node{{Type: "text", Text: text, Style: []string{"muted"}}})
		b.c.Notice = "system"
		return b.finish()
	case "hongbao":
		var t struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		return summary("红包", "🧧", strings.Trim(t.Text, "[]"))
	case "vote":
		var t struct {
			Topic   string   `json:"topic"`
			Options []string `json:"options"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		b.addParagraph([]Node{{Type: "text", Text: "[投票]", Style: []string{"muted"}}, {Type: "text", Text: " 🗳️ " + t.Topic, Style: []string{"bold"}}})
		for i, o := range t.Options {
			b.addParagraph([]Node{{Type: "text", Text: fmt.Sprintf("%d. %s", i+1, o)}})
		}
		return b.finish()
	case "todo":
		var t struct {
			Summary json.RawMessage `json:"summary"`
			DueTime string          `json:"due_time"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		b.addParagraph([]Node{{Type: "text", Text: "[任务]", Style: []string{"muted"}}})
		if len(t.Summary) > 0 {
			title, rows := parsePostJSON(string(t.Summary))
			if title != "" {
				b.addParagraph([]Node{{Type: "text", Text: "✅ " + title, Style: []string{"bold"}}})
			}
			b.fromPostNodes(rows)
		}
		if d := fmtMillis(t.DueTime); d != "" {
			b.addParagraph([]Node{{Type: "text", Text: "截止时间：" + d, Style: []string{"muted"}}})
		}
		return b.finish()
	case "share_calendar_event", "calendar", "general_calendar":
		var t struct {
			Summary   string `json:"summary"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		kind := map[string]string{"share_calendar_event": "日程分享", "calendar": "日程邀请", "general_calendar": "日程"}[msgType]
		b.addParagraph([]Node{{Type: "text", Text: "[" + kind + "]", Style: []string{"muted"}}, {Type: "text", Text: " 📅 " + t.Summary, Style: []string{"bold"}}})
		if st := fmtMillis(t.StartTime); st != "" {
			when := st
			if et := fmtMillis(t.EndTime); et != "" {
				when += " ~ " + et
			}
			b.addParagraph([]Node{{Type: "text", Text: when, Style: []string{"muted"}}})
		}
		return b.finish()
	case "video_chat":
		var t struct {
			Topic     string `json:"topic"`
			StartTime string `json:"start_time"`
		}
		_ = json.Unmarshal([]byte(content), &t)
		return summary("视频会议", "📹 "+t.Topic, fmtMillis(t.StartTime))
	default:
		return placeholder("[暂不支持的消息类型: " + msgType + "]")
	}
	return b.finish()
}

var systemVarRe = regexp.MustCompile(`\{([A-Za-z_]+)\}`)

// systemText fills a system-message template ("{from_user} invited {to_chatters}…").
func systemText(tpl string, raw map[string]any) string {
	if tpl == "" {
		return ""
	}
	return strings.TrimSpace(systemVarRe.ReplaceAllStringFunc(tpl, func(v string) string {
		k := v[1 : len(v)-1]
		switch x := raw[k].(type) {
		case []any:
			var parts []string
			for _, e := range x {
				if s, ok := e.(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
			return strings.Join(parts, "、")
		case map[string]any:
			if i, ok := x["i18n_text"].(map[string]any); ok {
				for _, l := range []string{"zh_cn", "en_us"} {
					if s, ok := i[l].(string); ok && s != "" {
						return s
					}
				}
			}
			if s, ok := x["text"].(string); ok {
				return s
			}
		case string:
			return x
		}
		return ""
	}))
}

// parseInteractive handles msg_type=interactive in both forms:
//   - original card JSON (header / elements / body.elements), returned when the
//     API is called with card_msg_content_type=user_card_content;
//   - the simplified {"title","elements":[[…]]} structure returned by default,
//     where cards the platform cannot express are replaced by a "please upgrade
//     your client" placeholder.
func parseInteractive(b *builder, content string) Content {
	if card, ok := b.parseCardContent(content); ok {
		b.c.Card = card
		c := b.finish()
		c.Plain = cardPlain(card)
		return c
	}
	var card struct {
		Title    string          `json:"title"`
		Elements json.RawMessage `json:"elements"`
	}
	_ = json.Unmarshal([]byte(content), &card)
	b.c.Title = card.Title
	b.noDL = true // resources inside cards cannot be downloaded (234043)
	var rows [][]postNode
	if len(card.Elements) > 0 && json.Unmarshal(card.Elements, &rows) == nil {
		b.fromPostNodes(rows)
	}
	plain := strings.TrimSpace(strings.Join(b.plain, "\n"))
	if isCardPlaceholder(plain) {
		// Nothing but the platform placeholder: do not pretend it is content.
		b.c.Paragraphs, b.plain, b.c.Attachments = nil, nil, nil
		b.c.Notice = "card_unavailable"
		b.addParagraph([]Node{{Type: "text", Text: "[卡片消息]", Style: []string{"muted"}}})
		if card.Title != "" {
			b.c.Title = card.Title
		}
		return b.finish()
	}
	if len(b.c.Paragraphs) == 0 {
		b.addParagraph([]Node{{Type: "text", Text: "[卡片消息]", Style: []string{"muted"}}})
	}
	return b.finish()
}

// parsePostJSON accepts both the flat form {"title","content"} returned by the
// message APIs and the language-keyed form {"zh_cn":{...}} used when sending.
func parsePostJSON(content string) (string, [][]postNode) {
	type body struct {
		Title   string       `json:"title"`
		Content [][]postNode `json:"content"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return "", nil
	}
	if _, ok := raw["content"]; ok {
		var b body
		_ = json.Unmarshal([]byte(content), &b)
		return b.Title, b.Content
	}
	for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
		if r, ok := raw[lang]; ok {
			var b body
			if json.Unmarshal(r, &b) == nil {
				return b.Title, b.Content
			}
		}
	}
	for _, r := range raw {
		var b body
		if json.Unmarshal(r, &b) == nil && (b.Title != "" || len(b.Content) > 0) {
			return b.Title, b.Content
		}
	}
	return "", nil
}

// firstLine returns the first non-empty line of s, shortened to max runes.
func firstLine(s string, max int) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			return truncate(ln, max)
		}
	}
	return ""
}

// fileExt returns the lower-case extension of name without the dot.
func fileExt(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}
