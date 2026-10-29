package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MockAPI serves realistic sample data without any Feishu credentials.
type MockAPI struct {
	chats    []Chat
	chatMsgs map[string][]RawMessage // chat_id -> root + ordinary messages
	threads  map[string][]RawMessage // thread_id -> root + replies
	forwards map[string][]RawMessage // merge_forward message id -> sub-messages
	pageSize int
	auth     *Auth

	mu      sync.RWMutex
	sent    map[string]*RawMessage // reply uuid -> message (dedupe)
	nextSeq int

	driveNames    map[string]string // drive file token -> name
	driveModTimes map[string]int64  // drive file token -> modify timestamp ms
	driveFiles    map[string][]byte // drive file token -> custom content bytes
	lastStamp     int64
	// hideThreadRoot makes ListMessages(thread) omit the opening message,
	// matching the real Feishu thread container.
	hideThreadRoot map[string]bool
	sharedChats    []string
	sharedMembers  []string
	driveMembers   map[string][]DrivePermissionMember
	privateClosed  map[string]bool
	linkShares     map[string]string
}

// gate mimics the real API: every call needs a valid user login.
func (m *MockAPI) gate(ctx context.Context) error {
	if m.auth == nil {
		return nil
	}
	_, err := m.auth.AccessToken(ctx)
	return err
}

// mockSpec describes one sample message of a thread.
type mockSpec struct {
	id, typ, sender string
	ago             int // minutes ago
	content         string
	mentions        []Mention
}

const (
	uZhang = "ou_mock_zhangwei"
	uLi    = "ou_mock_lina"
	uWang  = "ou_mock_wanglei"
	uChen  = "ou_mock_chenjing"
	appBot = "cli_mock_weeklybot"
)

var mockNames = map[string]string{uZhang: "张伟", uLi: "李娜", uWang: "王磊", uChen: "陈静"}

func jc(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func NewMockAPI(auth *Auth) *MockAPI {
	m := &MockAPI{
		auth:     auth,
		chatMsgs: map[string][]RawMessage{},
		threads:  map[string][]RawMessage{},
		forwards: map[string][]RawMessage{},
		sent:     map[string]*RawMessage{},
		pageSize: 4, // small on purpose: exercises pagination
		// a Drive file that is shared as a link in a thread (downloadable)
		driveNames:    map[string]string{"boxmockseed01": "发版checklist.txt"},
		driveModTimes: map[string]int64{"boxmockseed01": 1760000000000},
		driveFiles:    map[string][]byte{},
		linkShares:    map[string]string{},
	}
	m.chats = []Chat{
		{ChatID: "oc_mock_dev", Name: "产品研发讨论群", Description: "研发日常讨论"},
		{ChatID: "oc_mock_proj", Name: "客户项目交付群", Description: "项目交付与验收"},
		{ChatID: "oc_mock_design", Name: "设计评审", Description: "视觉与交互评审"},
		{ChatID: "oc_mock_bot", Name: "机器人与自动化", Description: "卡片 / 合并转发 / 各类消息样例"},
		{ChatID: "oc_mock_empty", Name: "闲聊群（无话题）"},
	}
	now := time.Now()
	ts := func(minAgo int) string {
		return strconv.FormatInt(now.Add(-time.Duration(minAgo)*time.Minute).UnixMilli(), 10)
	}
	mention := func(key, id string) Mention { return Mention{Key: key, ID: id, IDType: "open_id", Name: mockNames[id]} }

	type spec = mockSpec
	// addThread builds a thread: the first spec is the root.
	addThread := func(chat, thread string, specs []spec) {
		rootID := specs[0].id
		for i, sp := range specs {
			rm := RawMessage{
				MessageID: sp.id, MsgType: sp.typ, CreateTime: ts(sp.ago), UpdateTime: ts(sp.ago),
				ChatID: chat, ThreadID: thread, Mentions: sp.mentions,
			}
			rm.Body.Content = sp.content
			rm.Sender.ID = sp.sender
			rm.Sender.IDType = "open_id"
			rm.Sender.SenderType = "user"
			if strings.HasPrefix(sp.sender, "cli_") {
				rm.Sender.IDType = "app_id"
				rm.Sender.SenderType = "app"
			}
			if i > 0 {
				rm.RootID = rootID
				rm.ParentID = rootID
			}
			m.threads[thread] = append(m.threads[thread], rm)
			if i == 0 {
				m.chatMsgs[chat] = append(m.chatMsgs[chat], rm)
			}
		}
	}
	plain := func(chat, id, sender string, ago int, text string) {
		rm := RawMessage{MessageID: id, MsgType: "text", CreateTime: ts(ago), ChatID: chat}
		rm.Body.Content = jc(map[string]string{"text": text})
		rm.Sender.ID, rm.Sender.IDType, rm.Sender.SenderType = sender, "open_id", "user"
		m.chatMsgs[chat] = append(m.chatMsgs[chat], rm)
	}

	// ---- 产品研发讨论群 ----
	plain("oc_mock_dev", "om_mock_p1", uWang, 700, "早上好，今天站会 10:30。")
	addThread("oc_mock_dev", "omt_mock_001", []spec{
		{"om_mock_a01", "text", uZhang, 190, jc(map[string]string{"text": "登录页在 iOS 17 上偶发白屏，大家有头绪吗？\n@_user_1 你昨天改过路由守卫，帮忙看下"}), []Mention{mention("@_user_1", uLi)}},
		{"om_mock_a02", "text", uLi, 180, jc(map[string]string{"text": "我看了下日志，疑似 token 刷新失败后没有兜底跳转。"}), nil},
		{"om_mock_a03", "image", uLi, 176, jc(map[string]string{"image_key": "img_mock_console_error"}), nil},
		{"om_mock_a04", "post", uWang, 160, jc(map[string]any{"title": "临时修复方案", "content": [][]map[string]any{
			{{"tag": "text", "text": "建议在拦截器里加兜底，", "style": []string{"bold"}}, {"tag": "text", "text": "参考 "}, {"tag": "a", "text": "接口文档", "href": "https://example.feishu.cn/docx/MockDocApi123"}, {"tag": "text", "text": "，cc "}, {"tag": "at", "user_id": "@_user_1"}},
			{{"tag": "code_block", "language": "JS", "text": "axios.interceptors.response.use(null, (err) => {\n  if (err.response?.status === 401) location.replace('/login')\n  return Promise.reject(err)\n})"}},
			{{"tag": "text", "text": "改完后效果："}, {"tag": "emotion", "emoji_type": "THUMBSUP"}},
			{{"tag": "img", "image_key": "img_mock_after_fix"}},
		}}), []Mention{mention("@_user_1", uZhang)}},
		{"om_mock_a05", "file", uLi, 120, jc(map[string]string{"file_key": "file_mock_log", "file_name": "ios17-whitescreen.log"}), nil},
		{"om_mock_a06", "text", uZhang, 30, jc(map[string]string{"text": "已合并到 release/2.8，感谢大家！🎉"}), nil},
	})
	addThread("oc_mock_dev", "omt_mock_002", []spec{
		{"om_mock_b01", "post", uChen, 400, jc(map[string]any{"title": "Q4 发版计划（草案）", "content": [][]map[string]any{
			{{"tag": "text", "text": "10 月 28 日：功能冻结"}},
			{{"tag": "text", "text": "11 月 05 日：灰度发布"}},
			{{"tag": "text", "text": "11 月 12 日：全量发布"}},
			{{"tag": "text", "text": "详细排期见 "}, {"tag": "a", "text": "发版排期表", "href": "https://example.feishu.cn/sheets/MockSheet456"}},
		}}), nil},
		{"om_mock_b02", "text", uWang, 380, jc(map[string]string{"text": "@_all 请各模块负责人本周五前确认范围。"}), []Mention{{Key: "@_all", ID: "all", Name: "所有人"}}},
		{"om_mock_b03", "file", uChen, 350, jc(map[string]string{"file_key": "file_mock_pdf", "file_name": "Q4发版风险评估.pdf"}), nil},
		{"om_mock_b04", "file", uZhang, 340, jc(map[string]string{"file_key": "file_mock_csv", "file_name": "q4_budget.csv"}), nil},
		{"om_mock_b05", "text", uLi, 300, jc(map[string]string{"text": "风险评估我看过了，第 3 页的回滚方案需要补充。参考：https://example.feishu.cn/wiki/MockWikiRollback789 以及 https://github.com/example/release-tools 。云空间版本：https://mock.feishu.cn/file/boxmockseed01"}), nil},
	})
	addThread("oc_mock_dev", "omt_mock_003", []spec{
		{"om_mock_c01", "text", uWang, 2900, jc(map[string]string{"text": "接口文档 v2 更新了： https://example.feishu.cn/docx/MockDocApi123"}), nil},
		{"om_mock_c02", "text", uChen, 2880, jc(map[string]string{"text": "收到，我这边同步前端。"}), nil},
	})

	// ---- 客户项目交付群 ----
	addThread("oc_mock_proj", "omt_mock_101", []spec{
		{"om_mock_d01", "file", uChen, 1500, jc(map[string]string{"file_key": "file_mock_pdf", "file_name": "客户验收材料-V3.pdf"}), nil},
		{"om_mock_d02", "text", uZhang, 1480, jc(map[string]string{"text": "客户反馈第二章的数据口径要再确认。"}), nil},
		{"om_mock_d03", "file", uChen, 1400, jc(map[string]string{"file_key": "file_mock_docx", "file_name": "验收报告模板.docx"}), nil},
		{"om_mock_d04", "media", uWang, 1300, jc(map[string]string{"file_key": "file_mock_video", "image_key": "img_mock_video_cover", "file_name": "功能演示.mp4"}), nil},
	})
	addThread("oc_mock_proj", "omt_mock_102", []spec{
		{"om_mock_e01", "interactive", appBot, 60, jc(map[string]any{"title": "【周报提醒】本周交付进度", "elements": [][]map[string]any{
			{{"tag": "text", "text": "已完成 8 / 12 项，请在周五 18:00 前更新进度。"}},
			{{"tag": "a", "text": "打开进度看板", "href": "https://example.feishu.cn/base/MockBase321"}},
		}}), nil},
		{"om_mock_e02", "text", uChen, 50, jc(map[string]string{"text": "收到，已更新。"}), nil},
	})
	plain("oc_mock_proj", "om_mock_p2", uZhang, 45, "（这是一条普通群消息，不属于任何话题，不应出现在话题列表中）")

	// ---- 设计评审 ----
	addThread("oc_mock_design", "omt_mock_201", []spec{
		{"om_mock_f01", "post", uLi, 900, jc(map[string]any{"title": "新版首页视觉稿评审", "content": [][]map[string]any{
			{{"tag": "text", "text": "首页 Banner 与卡片布局的两版方案，欢迎评审："}},
			{{"tag": "img", "image_key": "img_mock_design_a"}},
			{{"tag": "img", "image_key": "img_mock_design_b"}},
		}}), nil},
		{"om_mock_f02", "text", uZhang, 880, jc(map[string]string{"text": "B 方案信息层级更清晰，我倾向 B。"}), nil},
		{"om_mock_f03", "audio", uWang, 860, jc(map[string]any{"file_key": "file_mock_audio", "duration": 12000}), nil},
		{"om_mock_f04", "image", uChen, 840, jc(map[string]string{"image_key": "img_mock_restricted"}), nil},
	})

	// ---- 机器人与自动化: cards, merged-forward and every other message type ----
	mockCards(m, addThread, mention)
	return m
}

func (m *MockAPI) ListChats(ctx context.Context, pageToken string) (*ChatPage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	off, _ := strconv.Atoi(pageToken)
	end := off + 3
	if end > len(m.chats) {
		end = len(m.chats)
	}
	p := &ChatPage{Items: m.chats[off:end]}
	if end < len(m.chats) {
		p.HasMore, p.PageToken = true, strconv.Itoa(end)
	}
	return p, nil
}

func (m *MockAPI) ListMessages(ctx context.Context, containerType, containerID, pageToken string, desc bool) (*MsgPage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	var src []RawMessage
	m.mu.RLock()
	switch containerType {
	case "chat":
		src = append(src, m.chatMsgs[containerID]...)
	case "thread":
		for _, msg := range m.threads[containerID] {
			if m.hideThreadRoot[containerID] && isRoot(msg) {
				continue
			}
			src = append(src, msg)
		}
	default:
		m.mu.RUnlock()
		return nil, &FeishuError{Code: 230001, Msg: "invalid container_id_type", HTTPStatus: 400}
	}
	m.mu.RUnlock()
	if containerType == "thread" && len(src) == 0 {
		return nil, &FeishuError{Code: 230073, Msg: "The thread is invisible to the operator.", HTTPStatus: 400}
	}
	sort.SliceStable(src, func(i, j int) bool {
		a, b := msgTime(src[i]), msgTime(src[j])
		if desc && containerType == "chat" {
			return a > b
		}
		return a < b
	})
	off, _ := strconv.Atoi(pageToken)
	if off > len(src) {
		off = len(src)
	}
	end := off + m.pageSize
	if end > len(src) {
		end = len(src)
	}
	p := &MsgPage{Items: src[off:end]}
	if end < len(src) {
		p.HasMore, p.PageToken = true, strconv.Itoa(end)
	}
	return p, nil
}

func (m *MockAPI) BatchUsers(ctx context.Context, ids []string) ([]User, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	var out []User
	for _, id := range ids {
		if n, ok := mockNames[id]; ok {
			av := ""
			if id != uChen { // leave one user without avatar to test the initials fallback
				av = "/api/mock/avatar?name=" + n + "&id=" + id
			}
			out = append(out, User{OpenID: id, Name: n, Avatar: av})
		}
	}
	return out, nil
}

func (m *MockAPI) Download(ctx context.Context, messageID, fileKey, resType string) (*Download, error) {
	bin := func(b []byte, ct, name string) (*Download, error) {
		return &Download{Body: io.NopCloser(bytes.NewReader(b)), ContentType: ct, Filename: name}, nil
	}
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	switch {
	case fileKey == "img_mock_restricted":
		// simulates the real limitation: resource API is app-identity only
		return nil, &FeishuError{Code: 234004, HTTPStatus: 400, Msg: "应用身份(tenant_access_token): 234004 App not in chat.；用户身份(user_access_token): 99991668 Invalid access token"}
	case strings.HasPrefix(fileKey, "img_mock_"):
		return bin(mockPNG(fileKey), "image/png", fileKey+".png")
	case fileKey == "file_mock_pdf":
		return bin(mockPDF("Mock PDF document - feishu-topics"), "application/octet-stream", "")
	case fileKey == "file_mock_csv":
		return bin([]byte("\xef\xbb\xbf部门,预算(万元),已用\n研发,320,210\n测试,80,55\n运维,120,98\n"), "text/csv", "q4_budget.csv")
	case fileKey == "file_mock_log":
		return bin([]byte("2026-10-10 09:12:01 WARN  token refresh failed: 401\n2026-10-10 09:12:01 ERROR router guard: no fallback\n"), "application/octet-stream", "ios17-whitescreen.log")
	case fileKey == "file_mock_docx":
		return bin([]byte("PK\x03\x04 mock docx bytes (not a real document)"), "application/octet-stream", "验收报告模板.docx")
	case fileKey == "file_mock_video":
		return bin([]byte("mock video bytes"), "video/mp4", "功能演示.mp4")
	case fileKey == "file_mock_audio":
		return bin([]byte("mock audio bytes"), "audio/ogg", "voice.opus")
	}
	return nil, &FeishuError{Code: 234003, Msg: "File not in message.", HTTPStatus: 400}
}

// mockPNG draws a deterministic gradient "UI screenshot" for the given key.
func mockPNG(seed string) []byte {
	h := fnv.New32a()
	h.Write([]byte(seed))
	v := h.Sum32()
	r0, g0, b0 := uint8(v), uint8(v>>8), uint8(v>>16)
	const w, hgt = 720, 420
	img := image.NewRGBA(image.Rect(0, 0, w, hgt))
	for y := 0; y < hgt; y++ {
		for x := 0; x < w; x++ {
			t := float64(x+y) / float64(w+hgt)
			img.Set(x, y, color.RGBA{uint8(float64(r0)*(1-t) + 40*t), uint8(float64(g0)*(1-t) + 60*t), uint8(float64(b0)*(1-t) + 120*t), 255})
		}
	}
	rect := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.Set(x, y, c)
			}
		}
	}
	rect(0, 0, w, 36, color.RGBA{30, 32, 40, 255})
	for i := 0; i < 3; i++ {
		rect(14+i*20, 12, 24+i*20, 22, color.RGBA{uint8(220 - i*40), uint8(90 + i*60), 90, 255})
	}
	rect(30, 70, 250, 380, color.RGBA{255, 255, 255, 235})
	for i := 0; i < 6; i++ {
		rect(46, 92+i*46, 46+int(120+(v>>uint(i))%80), 108+i*46, color.RGBA{200, 205, 220, 255})
	}
	rect(280, 70, 690, 200, color.RGBA{255, 255, 255, 220})
	for i := 0; i < 8; i++ {
		hh := 20 + int((v>>uint(i*3))%90)
		rect(300+i*47, 190-hh, 330+i*47, 190, color.RGBA{60, uint8(120 + i*12), 220, 255})
	}
	rect(280, 220, 690, 380, color.RGBA{20, 22, 28, 235})
	for i := 0; i < 7; i++ {
		rect(298, 240+i*20, 298+int(80+(v>>uint(i+2))%260), 250+i*20, color.RGBA{uint8(90 + i*20), 200, 140, 255})
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// mockPDF writes a minimal valid one-page PDF.
func mockPDF(text string) []byte {
	var buf bytes.Buffer
	var offs []int
	obj := func(s string) {
		offs = append(offs, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offs), s)
	}
	buf.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	stream := fmt.Sprintf("BT /F1 22 Tf 60 760 Td (%s) Tj ET\nBT /F1 12 Tf 60 720 Td (This PDF is generated by mock mode.) Tj ET", strings.NewReplacer("(", "", ")", "", "\\", "").Replace(text))
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return buf.Bytes()
}

func handleMockAvatar(w http.ResponseWriter, r *http.Request) {
	name := []rune(r.URL.Query().Get("name"))
	initial := "?"
	if len(name) > 0 {
		initial = string(name[len(name)-1:])
	}
	h := fnv.New32a()
	h.Write([]byte(r.URL.Query().Get("id")))
	hue := h.Sum32() % 360
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="72" height="72"><rect width="72" height="72" fill="hsl(%d,55%%,50%%)"/><text x="36" y="48" font-size="34" text-anchor="middle" fill="#fff" font-family="sans-serif">%s</text></svg>`, hue, html.EscapeString(initial))
}
