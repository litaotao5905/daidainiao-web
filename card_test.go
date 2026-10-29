package main

import (
	"strings"
	"testing"
)

func findBlock(bs []CardBlock, typ string) *CardBlock {
	for i := range bs {
		if bs[i].Type == typ {
			return &bs[i]
		}
	}
	return nil
}

func TestRawCardV1(t *testing.T) {
	js := `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"构建结果"},"subtitle":{"tag":"plain_text","content":"CI #1"},"template":"green","text_tag_list":[{"text":{"content":"成功"}}]},
	"elements":[
	 {"tag":"div","fields":[{"is_short":true,"text":{"tag":"lark_md","content":"**分支**\nmain"}},{"is_short":false,"text":{"tag":"plain_text","content":"备注"}}]},
	 {"tag":"markdown","content":"**结果**：<font color='green'>通过</font> 见 [报告](https://x.feishu.cn/docx/Abc) <at id=ou_1></at> :DONE:"},
	 {"tag":"hr"},
	 {"tag":"img","img_key":"img_v2_abc","alt":{"content":"趋势"}},
	 {"tag":"note","elements":[{"tag":"plain_text","content":"自动发送"}]},
	 {"tag":"action","actions":[{"tag":"button","text":{"content":"打开"},"type":"primary","url":"https://example.com/a"},{"tag":"button","text":{"content":"回调"},"value":{"a":1}}]}
	]}`
	c := ParseContent("interactive", js, []Mention{{Key: "@_user_1", ID: "ou_1", Name: "张三"}})
	if c.Card == nil {
		t.Fatalf("card not parsed: %+v", c)
	}
	k := c.Card
	if k.Title != "构建结果" || k.Subtitle != "CI #1" || k.Template != "green" || len(k.Tags) != 1 || k.Tags[0] != "成功" {
		t.Fatalf("header: %+v", k)
	}
	f := findBlock(k.Blocks, "fields")
	if f == nil || len(f.Children) != 2 || !f.Children[0].Short || f.Children[1].Short {
		t.Fatalf("fields: %+v", f)
	}
	md := findBlock(k.Blocks, "text")
	if md == nil {
		t.Fatal("no text block")
	}
	var sawGreen, sawLink, sawAt, sawEmoji bool
	for _, p := range md.Paras {
		for _, n := range p {
			switch {
			case n.Type == "text" && n.Text == "通过":
				for _, s := range n.Style {
					if s == "c-green" {
						sawGreen = true
					}
				}
			case n.Type == "link" && n.Href == "https://x.feishu.cn/docx/Abc" && n.IsDoc && n.Text == "报告":
				sawLink = true
			case n.Type == "at" && n.Text == "@张三":
				sawAt = true
			case n.Type == "emoji" && n.Text == "✅":
				sawEmoji = true
			}
		}
	}
	if !sawGreen || !sawLink || !sawAt || !sawEmoji {
		t.Fatalf("markdown: green=%v link=%v at=%v emoji=%v paras=%+v", sawGreen, sawLink, sawAt, sawEmoji, md.Paras)
	}
	if findBlock(k.Blocks, "hr") == nil || findBlock(k.Blocks, "note") == nil {
		t.Fatal("hr/note missing")
	}
	img := findBlock(k.Blocks, "image")
	if img == nil || img.Key != "img_v2_abc" || img.Title != "趋势" {
		t.Fatalf("image: %+v", img)
	}
	act := findBlock(k.Blocks, "actions")
	if act == nil || len(act.Buttons) != 2 || act.Buttons[0].URL != "https://example.com/a" || act.Buttons[1].URL != "" {
		t.Fatalf("actions: %+v", act)
	}
	// card images are attachments that must not be fetched
	var imgAtt *Attachment
	var linkAtts int
	for i := range c.Attachments {
		if c.Attachments[i].Kind == "image" {
			imgAtt = &c.Attachments[i]
		} else {
			linkAtts++
		}
	}
	if imgAtt == nil || !imgAtt.NoDownload || linkAtts < 2 {
		t.Fatalf("attachments: %+v", c.Attachments)
	}
	if !strings.HasPrefix(c.Plain, "构建结果") || !strings.Contains(c.Plain, "[打开]") {
		t.Fatalf("plain: %q", c.Plain)
	}
}

func TestRawCardV2(t *testing.T) {
	js := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"🧵 任务"},"template":"blue"},"body":{"elements":[
	 {"tag":"markdown","content":"# 标题\n- 项一\n- 项二\n\n` + "```go" + `\nfmt.Println(1)\n` + "```" + `"},
	 {"tag":"column_set","columns":[{"tag":"column","weight":2,"elements":[{"tag":"markdown","content":"左"}]},{"tag":"column","width":"auto","elements":[{"tag":"markdown","content":"右"}]}]},
	 {"tag":"collapsible_panel","expanded":true,"header":{"title":{"tag":"markdown","content":"**明细**"}},"elements":[
	   {"tag":"table","columns":[{"name":"a","display_name":"步骤"},{"name":"b","display_name":"耗时"}],"rows":[{"a":"检索","b":"1m"},{"a":"汇总","b":12}]}]},
	 {"tag":"button","text":{"content":"去看看"},"behaviors":[{"type":"open_url","default_url":"https://example.com/x"}]},
	 {"tag":"mystery_widget","text":{"tag":"plain_text","content":"未知组件里的文字"}}
	]}}`
	c := ParseContent("interactive", js, nil)
	if c.Card == nil || c.Card.Title != "🧵 任务" {
		t.Fatalf("card: %+v", c.Card)
	}
	bs := c.Card.Blocks
	md := findBlock(bs, "text")
	var sawBold, sawBullet, sawCode bool
	for _, p := range md.Paras {
		for _, n := range p {
			if n.Type == "text" && n.Text == "标题" && len(n.Style) == 1 && n.Style[0] == "bold" {
				sawBold = true
			}
			if n.Type == "text" && strings.HasPrefix(n.Text, "• 项") {
				sawBullet = true
			}
			if n.Type == "code" && n.Text == "fmt.Println(1)" && n.Lang == "go" {
				sawCode = true
			}
		}
	}
	if !sawBold || !sawBullet || !sawCode {
		t.Fatalf("md blocks: %v %v %v %+v", sawBold, sawBullet, sawCode, md.Paras)
	}
	cols := findBlock(bs, "columns")
	if cols == nil || len(cols.Cols) != 2 || cols.Weights[0] != 2 || cols.Weights[1] != 0 {
		t.Fatalf("columns: %+v", cols)
	}
	col := findBlock(bs, "collapse")
	if col == nil || col.Title != "明细" || !col.Open || len(col.Children) != 1 {
		t.Fatalf("collapse: %+v", col)
	}
	tb := col.Children[0]
	if tb.Type != "table" || len(tb.Header) != 2 || len(tb.Rows) != 2 || tb.Rows[1][1] != "12" {
		t.Fatalf("table: %+v", tb)
	}
	act := findBlock(bs, "actions")
	if act == nil || act.Buttons[0].URL != "https://example.com/x" {
		t.Fatalf("v2 button: %+v", act)
	}
	if !strings.Contains(c.Plain, "未知组件里的文字") {
		t.Fatalf("unknown component text lost: %q", c.Plain)
	}
}

func TestCardPlaceholderAndSimplifiedForm(t *testing.T) {
	// the platform placeholder is not content
	c := ParseContent("interactive", `{"title":"","elements":[[{"tag":"text","text":"请升级至最新版本客户端，以查看内容"}],[{"tag":"img","image_key":"img_v2_x"}]]}`, nil)
	if c.Notice != "card_unavailable" || c.Card != nil || strings.Contains(c.Plain, "请升级") || len(c.Attachments) != 0 {
		t.Fatalf("placeholder: %+v", c)
	}
	c2 := ParseContent("interactive", `{"title":"","elements":[[{"tag":"img","image_key":"img_v3_x"},{"tag":"text","text":"请将客户端升级至最新版本，以查看表格内容"}]]}`, nil)
	if c2.Notice != "card_unavailable" || c2.Card != nil || strings.Contains(c2.Plain, "升级") || len(c2.Attachments) != 0 {
		t.Fatalf("placeholder 2: %+v", c2)
	}
	// the simplified structure with real content still renders as paragraphs;
	// its images are flagged as non-downloadable
	c = ParseContent("interactive", `{"title":"提醒","elements":[[{"tag":"text","text":"进度 8/12"}],[{"tag":"img","image_key":"img_v2_y"}]]}`, nil)
	if c.Card != nil || c.Title != "提醒" || !strings.Contains(c.Plain, "进度 8/12") {
		t.Fatalf("simplified: %+v", c)
	}
	var sawNoDL bool
	for _, p := range c.Paragraphs {
		for _, n := range p {
			if n.Type == "image" && n.NoDL {
				sawNoDL = true
			}
		}
	}
	if !sawNoDL || len(c.Attachments) != 1 || !c.Attachments[0].NoDownload {
		t.Fatalf("card image must be NoDL: %+v", c)
	}
}

func TestOtherMessageTypes(t *testing.T) {
	opts := ParseOpts{
		AppLink:  "https://applink.feishu.cn",
		ChatName: func(id string) string { return map[string]string{"oc_1": "设计评审"}[id] },
		UserName: func(id string) string { return map[string]string{"ou_9": "陈静"}[id] },
	}
	cases := []struct {
		typ, content string
		want         []string
	}{
		{"system", `{"template":"{from_user} 邀请 {to_chatters} 加入了群聊","from_user":["张伟"],"to_chatters":["陈静","王磊"]}`, []string{"张伟 邀请 陈静、王磊 加入了群聊"}},
		{"system", `{"template":"{divider_text}","divider_text":{"text":"New","i18n_text":{"zh_cn":"新话题"}}}`, []string{"新话题"}},
		{"share_chat", `{"chat_id":"oc_1"}`, []string{"群名片", "设计评审", "在飞书中打开该群"}},
		{"share_user", `{"user_id":"ou_9"}`, []string{"个人名片", "陈静"}},
		{"todo", `{"task_id":"t","summary":{"title":"写周报","content":[[{"tag":"text","text":"本周进展"}]]},"due_time":"1893456000000"}`, []string{"任务", "写周报", "本周进展", "截止时间"}},
		{"vote", `{"topic":"周几开会","options":["周二","周三"]}`, []string{"投票", "周几开会", "1. 周二", "2. 周三"}},
		{"hongbao", `{"text":"恭喜发财"}`, []string{"红包", "恭喜发财"}},
		{"location", `{"name":"上海","longitude":"121.5","latitude":"31.2"}`, []string{"位置", "上海", "31.2"}},
		{"calendar", `{"summary":"评审","start_time":"1893456000000","end_time":"1893459600000"}`, []string{"日程邀请", "评审", "2030"}},
		{"video_chat", `{"topic":"同步会","start_time":"1893456000000"}`, []string{"视频会议", "同步会"}},
		{"sticker", `{"file_key":"f"}`, []string{"表情包"}},
		{"folder", `{"file_key":"f","file_name":"资料"}`, []string{"文件夹", "资料"}},
		{"merge_forward", `{"content":"Merged and Forwarded Message"}`, []string{"合并转发"}},
	}
	for _, tc := range cases {
		c := ParseContentOpts(tc.typ, tc.content, nil, opts)
		for _, w := range tc.want {
			if !strings.Contains(c.Plain, w) {
				t.Errorf("%s: plain %q lacks %q", tc.typ, c.Plain, w)
			}
		}
		if strings.Contains(c.Plain, "暂不支持") {
			t.Errorf("%s still reported as unsupported: %q", tc.typ, c.Plain)
		}
	}
	// share_chat link goes to the AppLink chat page
	c := ParseContentOpts("share_chat", `{"chat_id":"oc_1"}`, nil, opts)
	var href string
	for _, p := range c.Paragraphs {
		for _, n := range p {
			if n.Type == "link" {
				href = n.Href
			}
		}
	}
	if href != "https://applink.feishu.cn/client/chat/open?openChatId=oc_1" {
		t.Fatalf("share_chat link = %q", href)
	}
	if c := ParseContent("system", `{"template":"x"}`, nil); c.Notice != "system" {
		t.Fatalf("system notice = %q", c.Notice)
	}
}

func TestTextMarkdownLinkAndAttachments(t *testing.T) {
	c := ParseContent("text", `{"text":"见[开放平台](https://open.feishu.cn)和 https://a.com/x."}`, nil)
	var links []Node
	for _, n := range c.Paragraphs[0] {
		if n.Type == "link" {
			links = append(links, n)
		}
	}
	if len(links) != 2 || links[0].Text != "开放平台" || links[0].Href != "https://open.feishu.cn" || links[1].Href != "https://a.com/x" {
		t.Fatalf("links: %+v", links)
	}
	if len(c.Attachments) != 2 || c.Attachments[0].Name != "开放平台" {
		t.Fatalf("attachments: %+v", c.Attachments)
	}
}

func TestPostMarkdownTag(t *testing.T) {
	c := ParseContent("post", `{"title":"t","content":[[{"tag":"md","text":"**粗** 与 [链接](https://a.com)"}]]}`, nil)
	if !strings.Contains(c.Plain, "粗 与 链接") {
		t.Fatalf("md tag: %q", c.Plain)
	}
}

func TestRichMarkdownTableAndLists(t *testing.T) {
	js := `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"**目录调整思路**\n- 一级要点\n    - 二级要点\n| 章 | 标题 | 调整要点 |\n| -------- | -------- | -------- |\n| 01 | 现状与痛点 | **细化**痛点 |\n| 02 | 发展要求 | 155规划 |\n1. **第一项**：说明"}]}}`
	c := ParseContent("interactive", js, nil)
	if c.Card == nil || len(c.Card.Blocks) == 0 {
		t.Fatalf("card not parsed: %+v", c)
	}
	paras := c.Card.Blocks[0].Paras
	var sawSec, sawUL0, sawUL1, sawTable, sawOL bool
	for _, p := range paras {
		if len(p) == 0 {
			continue
		}
		n0 := p[0]
		switch {
		case n0.Block == "sec" && n0.Text == "目录调整思路":
			sawSec = true
		case n0.Block == "ul" && n0.Indent == 0:
			sawUL0 = true
		case n0.Block == "ul" && n0.Indent == 1:
			sawUL1 = true
		case n0.Type == "table" && len(n0.Cells) == 3 && len(n0.Cells[0]) == 3:
			sawTable = true
		case n0.Block == "ol" && n0.Marker == "1.":
			sawOL = true
		}
	}
	if !sawSec || !sawUL0 || !sawUL1 || !sawTable || !sawOL {
		t.Fatalf("missing rich elements: sec=%v ul0=%v ul1=%v table=%v ol=%v paras=%+v", sawSec, sawUL0, sawUL1, sawTable, sawOL, paras)
	}

	// Verify Feishu doc URL with attached Chinese suffix is split cleanly
	postJS := `{"title":"","content":[[{"tag":"a","href":"https://u0vocx8xrmg.feishu.cn/file/ZGc3bLTpyoTtdExjNeTcyki2nXb这一版来调整","text":"https://u0vocx8xrmg.feishu.cn/file/ZGc3bLTpyoTtdExjNeTcyki2nXb这一版来调整"}]]}`
	cp := ParseContent("post", postJS, nil)
	if len(cp.Attachments) != 1 || cp.Attachments[0].URL != "https://u0vocx8xrmg.feishu.cn/file/ZGc3bLTpyoTtdExjNeTcyki2nXb" {
		t.Fatalf("doc link tail not cleaned: %+v", cp.Attachments)
	}
}
