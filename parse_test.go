package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCloudLiteralLinksStayDiscoverable(t *testing.T) {
	for _, text := range []string{"https://x.feishu.cn/file/test1", "https://x.feishu.cn/wiki/test2"} {
		msg := assemble("请读取 "+text+" 然后查看 https://example.com/page", nil, nil)[0]
		if msg.Type != "post" {
			t.Fatal("cloud link was sent as auto-authorizing plain text")
		}
		c := ParseContent(msg.Type, msg.Content, nil)
		if len(c.Attachments) != 2 || c.Attachments[0].URL != text || c.Attachments[1].Kind != "link" {
			t.Fatalf("link discovery: %+v", c.Attachments)
		}
		var body any
		json.Unmarshal([]byte(msg.Content), &body)
		if strings.Contains(msg.Content, `"href":"`+text+`"`) {
			t.Fatal("cloud URL remains a normal hyperlink")
		}
	}
	c := ParseContent("post", `{"content":[[{"tag":"code_block","text":"print(\"https://x.feishu.cn/file/test1\")"}]]}`, nil)
	if len(c.Attachments) != 0 {
		t.Fatal("example code must not trigger file downloads")
	}
}

func TestParseTextWithMentionAndLink(t *testing.T) {
	ms := []Mention{{Key: "@_user_1", ID: "ou_a", Name: "李娜"}}
	c := ParseContent("text", `{"text":"@_user_1 请看 https://x.feishu.cn/docx/AbC123, 谢谢\n第二行 @_all"}`, ms)
	if len(c.Paragraphs) != 2 {
		t.Fatalf("paragraphs=%d", len(c.Paragraphs))
	}
	p := c.Paragraphs[0]
	if p[0].Type != "at" || p[0].Text != "@李娜" || p[0].UserID != "ou_a" {
		t.Fatalf("bad at node: %+v", p[0])
	}
	var link *Node
	for i := range p {
		if p[i].Type == "link" {
			link = &p[i]
		}
	}
	if link == nil || link.Href != "https://x.feishu.cn/docx/AbC123" || !link.IsDoc {
		t.Fatalf("bad link: %+v", link)
	}
	if c.Paragraphs[1][len(c.Paragraphs[1])-1].Text != "@所有人" {
		t.Fatalf("@_all not resolved: %+v", c.Paragraphs[1])
	}
	if len(c.Attachments) != 1 || c.Attachments[0].Kind != "doc" {
		t.Fatalf("attachments: %+v", c.Attachments)
	}
	if !strings.HasPrefix(c.Plain, "@李娜 请看") {
		t.Fatalf("plain=%q", c.Plain)
	}
}

func TestParsePostFlatAndLangKeyed(t *testing.T) {
	flat := `{"title":"周报","content":[[{"tag":"text","text":"本周 ","style":["bold"]},{"tag":"a","text":"看板","href":"https://example.com/a"},{"tag":"at","user_id":"@_user_1"}],[{"tag":"img","image_key":"img_v2_abc"}],[{"tag":"code_block","language":"GO","text":"x := 1"}],[{"tag":"emotion","emoji_type":"SMILE"}]]}`
	ms := []Mention{{Key: "@_user_1", ID: "ou_b", Name: "王磊"}}
	c := ParseContent("post", flat, ms)
	if c.Title != "周报" || len(c.Paragraphs) != 4 {
		t.Fatalf("title=%q paras=%d", c.Title, len(c.Paragraphs))
	}
	if c.Paragraphs[0][0].Style[0] != "bold" || c.Paragraphs[0][2].Text != "@王磊" {
		t.Fatalf("row0: %+v", c.Paragraphs[0])
	}
	if c.Paragraphs[1][0].Type != "image" || c.Paragraphs[1][0].Key != "img_v2_abc" {
		t.Fatalf("img: %+v", c.Paragraphs[1])
	}
	if c.Paragraphs[2][0].Lang != "go" || c.Paragraphs[3][0].Text != "😊" {
		t.Fatalf("code/emoji: %+v %+v", c.Paragraphs[2], c.Paragraphs[3])
	}
	kinds := map[string]int{}
	for _, a := range c.Attachments {
		kinds[a.Kind]++
	}
	if kinds["image"] != 1 || kinds["link"] != 1 {
		t.Fatalf("attachments: %+v", c.Attachments)
	}

	keyed := `{"zh_cn":{"title":"T","content":[[{"tag":"text","text":"hi"}]]}}`
	c2 := ParseContent("post", keyed, nil)
	if c2.Title != "T" || len(c2.Paragraphs) != 1 || c2.Plain != "T\nhi" {
		t.Fatalf("keyed: %+v", c2)
	}
}

func TestParseMediaTypes(t *testing.T) {
	c := ParseContent("image", `{"image_key":"img_v2_1"}`, nil)
	if len(c.Attachments) != 1 || c.Attachments[0].ResType != "image" || c.Paragraphs[0][0].Type != "image" {
		t.Fatalf("image: %+v", c)
	}
	c = ParseContent("file", `{"file_key":"file_v2_1","file_name":"报告.pdf"}`, nil)
	if c.Attachments[0].Name != "报告.pdf" || c.Attachments[0].ResType != "file" || c.Plain != "[文件] 报告.pdf" {
		t.Fatalf("file: %+v", c)
	}
	c = ParseContent("media", `{"file_key":"file_v2_2","image_key":"img_v2_2","file_name":"demo.mp4"}`, nil)
	if c.Attachments[0].Kind != "video" || c.Attachments[0].CoverKey != "img_v2_2" {
		t.Fatalf("media: %+v", c)
	}
	// keys with path characters must be rejected
	c = ParseContent("image", `{"image_key":"../etc/passwd"}`, nil)
	if len(c.Attachments) != 0 {
		t.Fatalf("unsafe key accepted: %+v", c)
	}
}

func TestParseFallbacks(t *testing.T) {
	if c := ParseContent("text", `not json`, nil); c.Plain != "not json" {
		t.Fatalf("lenient text: %q", c.Plain)
	}
	if c := ParseContent("weird", `{}`, nil); !strings.Contains(c.Plain, "暂不支持") {
		t.Fatalf("unknown: %q", c.Plain)
	}
	card := `{"title":"审批","elements":[[{"tag":"text","text":"请处理"},{"tag":"button","text":"同意"}]]}`
	c := ParseContent("interactive", card, nil)
	if c.Title != "审批" || !strings.Contains(c.Plain, "请处理[同意]") {
		t.Fatalf("card: %+v", c)
	}
}

func TestFirstLineAndDocURL(t *testing.T) {
	if got := firstLine("\n  \n 标题行 \n第二行", 80); got != "标题行" {
		t.Fatalf("got %q", got)
	}
	if !isDocURL("https://foo.feishu.cn/wiki/xyz") || isDocURL("https://example.com/docx/1") || isDocURL("javascript:alert(1)") {
		t.Fatal("isDocURL misclassified")
	}
}
