package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// mockCards adds the "机器人与自动化" chat: interactive cards (schema 1.0 / 2.0
// and the simplified placeholder form), a merged-forward message and one
// message of every other type the UI can summarise.
func mockCards(m *MockAPI, add func(chat, thread string, specs []mockSpec), mention func(key, id string) Mention) {
	const chat = "oc_mock_bot"
	cardCI := `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"📊 构建结果：release/2.8"},"subtitle":{"tag":"plain_text","content":"CI · 流水线 #1287"},"template":"green","text_tag_list":[{"tag":"text_tag","text":{"tag":"plain_text","content":"成功"},"color":"green"}]},` +
		`"elements":[{"tag":"div","fields":[{"is_short":true,"text":{"tag":"lark_md","content":"**分支**\nrelease/2.8"}},{"is_short":true,"text":{"tag":"lark_md","content":"**耗时**\n4 分 12 秒"}}]},` +
		`{"tag":"markdown","content":"**结果**：<font color='green'>全部通过</font>\n- 单元测试 ` + "`1,284`" + ` 通过\n- 覆盖率 *82.4%*\n负责人：<at id=ou_mock_zhangwei></at> 请确认，详情见 [构建报告](https://example.feishu.cn/docx/MockDocApi123)"},` +
		`{"tag":"hr"},{"tag":"img","img_key":"img_mock_chart","alt":{"tag":"plain_text","content":"覆盖率趋势"}},` +
		`{"tag":"note","elements":[{"tag":"plain_text","content":"由 CI 机器人自动发送 · "},{"tag":"lark_md","content":"[查看规则](https://example.feishu.cn/wiki/MockWikiRollback789)"}]},` +
		`{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"打开流水线"},"type":"primary","url":"https://example.feishu.cn/base/MockBase321"},{"tag":"button","text":{"tag":"plain_text","content":"重新运行"},"type":"default","value":{"k":"rerun"}}]}]}`
	cardDone := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"✅ 已完成"},"template":"green"},"body":{"elements":[{"tag":"markdown","content":"已完成，用时 2 分 35 秒。"}]}}`
	cardTasks := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"🧵 后台任务 1/1 已全部结束"},"template":"blue"},"body":{"elements":[` +
		`{"tag":"markdown","content":"✅ 搜索大全相关案例文档，以及九号SAP项目文档（后台命令）"},` +
		`{"tag":"column_set","columns":[{"tag":"column","width":"weighted","weight":1,"elements":[{"tag":"markdown","content":"**耗时**\n2 分 35 秒"}]},{"tag":"column","width":"weighted","weight":1,"elements":[{"tag":"markdown","content":"**命中文档**\n17 篇"}]}]},` +
		`{"tag":"collapsible_panel","expanded":false,"header":{"title":{"tag":"markdown","content":"**执行明细**"}},"elements":[{"tag":"table","columns":[{"name":"step","display_name":"步骤","data_type":"text"},{"name":"cost","display_name":"耗时","data_type":"text"}],"rows":[{"step":"检索","cost":"1m20s"},{"step":"汇总","cost":"1m15s"}]}]}]}}`
	cardThink := `{"schema":"2.0","header":{"title":{"tag":"plain_text","content":"💭 思考与进展"},"template":"grey"},"body":{"elements":[{"tag":"markdown","content":"✅ 本回合已完成，最终结论见下方消息。"}]}}`
	cardLost := `{"title":"","elements":[[{"tag":"text","text":"请升级至最新版本客户端，以查看内容"}],[{"tag":"img","image_key":"img_mock_card_lost"}]]}`

	add(chat, "omt_mock_301", []mockSpec{
		{"om_mock_g01", "text", uWang, 300, jc(map[string]string{"text": "@_user_1 帮我整理一下九号和大全的项目案例文档，整理完在这里汇报"}), []Mention{{Key: "@_user_1", ID: appBot, IDType: "app_id", Name: "呆呆鸟"}}},
		{"om_mock_g02", "interactive", appBot, 296, cardCI, nil},
		{"om_mock_g03", "interactive", appBot, 295, cardDone, nil},
		{"om_mock_g04", "interactive", appBot, 294, cardTasks, nil},
		{"om_mock_g05", "interactive", appBot, 293, cardThink, nil},
		{"om_mock_g06", "interactive", appBot, 292, cardLost, nil},
		{"om_mock_g07", "text", appBot, 290, jc(map[string]string{"text": "@_user_1 汇总完成，文档见 https://example.feishu.cn/docx/MockDocApi123"}), []Mention{mention("@_user_1", uWang)}},
	})

	// merged-forward: root message + sub-messages (one nested merge)
	now := time.Now()
	sub := func(id, typ, sender, upper string, ago int, content string) RawMessage {
		rm := RawMessage{MessageID: id, MsgType: typ, CreateTime: strconv.FormatInt(now.Add(-time.Duration(ago)*time.Minute).UnixMilli(), 10), ChatID: chat, UpperMessageID: upper}
		rm.Body.Content = content
		rm.Sender.ID, rm.Sender.IDType, rm.Sender.SenderType = sender, "open_id", "user"
		if strings.HasPrefix(sender, "cli_") {
			rm.Sender.IDType, rm.Sender.SenderType = "app_id", "app"
		}
		return rm
	}
	m.forwards["om_mock_h02"] = []RawMessage{
		sub("om_mock_h02a", "text", uZhang, "om_mock_h02", 500, jc(map[string]string{"text": "客户那边说验收报告要加一页回滚方案。"})),
		sub("om_mock_h02b", "image", uLi, "om_mock_h02", 498, jc(map[string]string{"image_key": "img_mock_after_fix"})),
		sub("om_mock_h02c", "interactive", appBot, "om_mock_h02", 497, cardDone),
		sub("om_mock_h02d", "merge_forward", uWang, "om_mock_h02", 495, jc(map[string]string{"content": "Merged and Forwarded Message"})),
		sub("om_mock_h02e", "text", uChen, "om_mock_h02d", 490, jc(map[string]string{"text": "（内层转发）回滚方案草稿见 https://example.feishu.cn/docx/MockDocApi123"})),
		sub("om_mock_h02f", "file", uChen, "om_mock_h02d", 489, jc(map[string]string{"file_key": "file_mock_pdf", "file_name": "回滚方案.pdf"})),
	}
	add(chat, "omt_mock_302", []mockSpec{
		{"om_mock_h01", "post", uLi, 480, jc(map[string]any{"title": "各类消息展示", "content": [][]map[string]any{{{"tag": "text", "text": "这个话题收集了接口返回的各种消息类型，用来检查渲染效果。"}}}}), nil},
		{"om_mock_h02", "merge_forward", uWang, 470, jc(map[string]string{"content": "Merged and Forwarded Message"}), nil},
		{"om_mock_h03", "system", uZhang, 460, jc(map[string]any{"template": "{from_user} 邀请 {to_chatters} 加入了群聊。", "from_user": []string{"张伟"}, "to_chatters": []string{"陈静", "王磊"}}), nil},
		{"om_mock_h04", "share_chat", uZhang, 450, jc(map[string]string{"chat_id": "oc_mock_design"}), nil},
		{"om_mock_h05", "share_user", uZhang, 440, jc(map[string]string{"user_id": uChen}), nil},
		{"om_mock_h06", "todo", uWang, 430, jc(map[string]any{"task_id": "t1", "summary": map[string]any{"title": "整理 Q4 风险清单", "content": [][]map[string]any{{{"tag": "text", "text": "覆盖研发、测试、运维三个方向"}}}}, "due_time": strconv.FormatInt(now.Add(72*time.Hour).UnixMilli(), 10)}), nil},
		{"om_mock_h07", "vote", uChen, 420, jc(map[string]any{"topic": "周会改到周几？", "options": []string{"周二", "周三", "周四"}}), nil},
		{"om_mock_h08", "hongbao", uZhang, 410, jc(map[string]string{"text": "恭喜发财，大吉大利"}), nil},
		{"om_mock_h09", "location", uLi, 400, jc(map[string]string{"name": "上海市浦东新区世纪大道 100 号", "longitude": "121.5", "latitude": "31.23"}), nil},
		{"om_mock_h10", "share_calendar_event", uChen, 390, jc(map[string]string{"summary": "Q4 发版评审", "start_time": strconv.FormatInt(now.Add(24*time.Hour).UnixMilli(), 10), "end_time": strconv.FormatInt(now.Add(25*time.Hour).UnixMilli(), 10)}), nil},
		{"om_mock_h11", "video_chat", uWang, 380, jc(map[string]string{"topic": "项目同步会", "start_time": strconv.FormatInt(now.Add(-6*time.Hour).UnixMilli(), 10)}), nil},
		{"om_mock_h12", "sticker", uLi, 370, jc(map[string]string{"file_key": "sticker_mock"}), nil},
		{"om_mock_h13", "folder", uChen, 360, jc(map[string]string{"file_key": "folder_mock", "file_name": "客户资料"}), nil},
		{"om_mock_h14", "text", uZhang, 350, jc(map[string]string{"text": "文档链接写法：[飞书开放平台](https://open.feishu.cn) 以及裸链接 https://github.com/example/release-tools"}), nil},
	})
}

// ---------- Reply / GetMessage ----------

var mockAtRe = regexp.MustCompile(`<at user_id="([^"]+)">([^<]*)</at>`)

// findLocked looks a message up in threads first (they hold the freshest
// copy), then in the flat chat history. Callers hold m.mu.
func (m *MockAPI) findLocked(id string) (RawMessage, bool) {
	for _, msgs := range m.threads {
		for _, x := range msgs {
			if x.MessageID == id {
				return x, true
			}
		}
	}
	for _, msgs := range m.chatMsgs {
		for _, x := range msgs {
			if x.MessageID == id {
				return x, true
			}
		}
	}
	return RawMessage{}, false
}

// GetMessage returns a message and, for merge_forward, its sub-messages.
func (m *MockAPI) GetMessage(ctx context.Context, messageID string) ([]RawMessage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if x, ok := m.findLocked(messageID); ok {
		return append([]RawMessage{x}, m.forwards[messageID]...), nil
	}
	return nil, &FeishuError{Code: 230001, Msg: "invalid message_id", HTTPStatus: 400}
}

// mockPrepare validates a message body the way the UI error paths need and
// converts <at> markup of text messages into the @_user_N form.
//
// Special texts let the UI's error paths be tried without credentials:
//
//	!scope  -> 99991679 (token lacks im:message.send_as_user)
//	!muted  -> 230035   (no permission to speak)
func mockPrepare(msgType, content string) (string, []Mention, error) {
	switch {
	case strings.Contains(content, "!scope"):
		return "", nil, &FeishuError{Code: 99991679, HTTPStatus: 400, Msg: "Unauthorized. You do not have permission to perform the requested operation on the resource. Please request user re-authorization and try again. required one of these privileges: [im:message, im:message.send_as_user]"}
	case strings.Contains(content, "!muted"):
		return "", nil, &FeishuError{Code: 230035, HTTPStatus: 400, Msg: "Send Message Permission deny."}
	}
	if msgType != "text" {
		return content, nil, nil
	}
	var body struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(content), &body)
	var mentions []Mention
	text := mockAtRe.ReplaceAllStringFunc(body.Text, func(tag string) string {
		g := mockAtRe.FindStringSubmatch(tag)
		key := fmt.Sprintf("@_user_%d", len(mentions)+1)
		mentions = append(mentions, Mention{Key: key, ID: g[1], IDType: "open_id", Name: g[2]})
		return key
	})
	return jc(map[string]string{"text": text}), mentions, nil
}

func (m *MockAPI) mockSender(rm *RawMessage) {
	rm.Sender.ID, rm.Sender.IDType, rm.Sender.SenderType = uLi, "open_id", "user"
	if m.auth != nil {
		if id := m.auth.UserID(); id != "" {
			rm.Sender.ID = id
		}
	}
}

// stampLocked returns a strictly increasing millisecond timestamp so that
// messages sent back to back keep their order.
func (m *MockAPI) stampLocked() string {
	t := time.Now().UnixMilli()
	if t <= m.lastStamp {
		t = m.lastStamp + 1
	}
	m.lastStamp = t
	return strconv.FormatInt(t, 10)
}

// ReplyMessage appends a reply to the in-memory thread; with inThread the
// replied-to message becomes the root of a new thread when it has none.
func (m *MockAPI) ReplyMessage(ctx context.Context, messageID, msgType, content, uuid string, inThread bool) (*RawMessage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	stored, mentions, err := mockPrepare(msgType, content)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if uuid != "" {
		if prev, ok := m.sent[uuid]; ok {
			cp := *prev
			return &cp, nil
		}
	}
	root, ok := m.findLocked(messageID)
	if !ok {
		return nil, &FeishuError{Code: 230001, Msg: "invalid message_id", HTTPStatus: 400}
	}
	m.nextSeq++
	thread := root.ThreadID
	if thread == "" {
		thread = fmt.Sprintf("omt_mock_new_%03d", m.nextSeq)
		root.ThreadID = thread
		msgs := m.chatMsgs[root.ChatID]
		for i := range msgs {
			if msgs[i].MessageID == root.MessageID {
				msgs[i].ThreadID = thread
			}
		}
		m.threads[thread] = []RawMessage{root}
	}
	rootID := root.MessageID
	if root.RootID != "" {
		rootID = root.RootID
	}
	now := m.stampLocked()
	rm := RawMessage{
		MessageID: fmt.Sprintf("om_mock_reply_%03d", m.nextSeq), RootID: rootID, ParentID: messageID, ThreadID: thread,
		MsgType: msgType, CreateTime: now, UpdateTime: now, ChatID: root.ChatID, Mentions: mentions,
	}
	rm.Body.Content = stored
	m.mockSender(&rm)
	m.threads[thread] = append(m.threads[thread], rm)
	if uuid != "" {
		cp := rm
		m.sent[uuid] = &cp
	}
	return &rm, nil
}

// SendMessage appends a new root message to the chat's flat history. Like the
// real API it carries no thread_id until somebody replies in thread.
func (m *MockAPI) SendMessage(ctx context.Context, chatID, msgType, content, uuid string) (*RawMessage, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	stored, mentions, err := mockPrepare(msgType, content)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if uuid != "" {
		if prev, ok := m.sent[uuid]; ok {
			cp := *prev
			return &cp, nil
		}
	}
	known := false
	for _, c := range m.chats {
		known = known || c.ChatID == chatID
	}
	if !known {
		return nil, &FeishuError{Code: 230002, HTTPStatus: 400, Msg: "The bot can not be outside the group."}
	}
	m.nextSeq++
	now := m.stampLocked()
	rm := RawMessage{
		MessageID: fmt.Sprintf("om_mock_root_%03d", m.nextSeq), MsgType: msgType,
		CreateTime: now, UpdateTime: now, ChatID: chatID, Mentions: mentions,
	}
	rm.Body.Content = stored
	m.mockSender(&rm)
	m.chatMsgs[chatID] = append(m.chatMsgs[chatID], rm)
	if uuid != "" {
		cp := rm
		m.sent[uuid] = &cp
	}
	return &rm, nil
}

// ---------- uploads ----------

// UploadIM simulates im/v1/images and im/v1/files. File names containing
// "nobot" fail like an app without bot capability (234007) and "!scope" like
// a token without im:resource, so the Drive fallback can be tried.
func (m *MockAPI) UploadIM(ctx context.Context, kind, name string, data []byte) (string, error) {
	if err := m.gate(ctx); err != nil {
		return "", err
	}
	switch {
	case strings.Contains(name, "nobot"):
		return "", &FeishuError{Code: 234007, HTTPStatus: 400, Msg: "App does not enable bot feature."}
	case strings.Contains(name, "!scope"):
		return "", &FeishuError{Code: 99991679, HTTPStatus: 400, Msg: "Unauthorized. You do not have permission to perform the requested operation on the resource. Please request user re-authorization and try again. required one of these privileges: [im:resource, im:resource:upload]"}
	case len(data) == 0:
		return "", &FeishuError{Code: 234010, HTTPStatus: 400, Msg: "File's size can't be 0."}
	}
	m.mu.Lock()
	m.nextSeq++
	n := m.nextSeq
	m.mu.Unlock()
	if kind == "image" {
		return fmt.Sprintf("img_up_mock_%03d", n), nil
	}
	return fmt.Sprintf("file_up_mock_%03d", n), nil
}

// DriveUpload simulates drive/v1/files/upload_all ("nodrive" in the name
// fails with a missing app scope).
func (m *MockAPI) DriveUpload(ctx context.Context, name string, data []byte) (string, error) {
	if err := m.gate(ctx); err != nil {
		return "", err
	}
	if strings.Contains(name, "nodrive") {
		return "", &FeishuError{Code: 1061073, HTTPStatus: 403, Msg: "no scope auth. required one of these privileges: [drive:drive, drive:file:upload]"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSeq++
	tok := fmt.Sprintf("boxmock%03d", m.nextSeq)
	if m.driveNames == nil {
		m.driveNames = map[string]string{}
	}
	m.driveNames[tok] = name
	return tok, nil
}

func (m *MockAPI) DriveURL(ctx context.Context, token string) (string, error) {
	if err := m.gate(ctx); err != nil {
		return "", err
	}
	return "https://mock.feishu.cn/file/" + token, nil
}

// DriveShareChat grants the given chat view, matching FeishuClient.
func (m *MockAPI) DriveShareChat(ctx context.Context, token, chatID string) error {
	_, err := m.DriveShareMember(ctx, token, "openchat", chatID, "view")
	return err
}

// DriveShareMember fails for files whose name contains "noshare".
func (m *MockAPI) DriveShareMember(ctx context.Context, token, memberType, memberID, perm string) (*DrivePermissionMember, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(m.driveNames[token], "noshare") {
		return nil, &FeishuError{Code: 1063002, HTTPStatus: 403, Msg: "Permission denied"}
	}
	if m.driveMembers == nil {
		m.driveMembers = map[string][]DrivePermissionMember{}
	}
	var member DrivePermissionMember
	if memberType == "openchat" {
		if !strings.HasPrefix(memberID, "oc_") {
			return nil, fmt.Errorf("群聊 ID 非法，停止授权")
		}
		member = DrivePermissionMember{ID: memberID, MemberType: "openchat", Perm: perm, Type: "chat"}
		m.sharedChats = append(m.sharedChats, memberID)
	} else {
		member = DrivePermissionMember{ID: "cli_mock_" + strings.TrimPrefix(memberID, "ou_"), MemberType: "appid", Perm: perm, Type: "app"}
		m.sharedMembers = append(m.sharedMembers, memberID)
	}
	m.driveMembers[token] = append(m.driveMembers[token], member)
	return &member, nil
}

func (m *MockAPI) SharedChats() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string{}, m.sharedChats...)
}

func (m *MockAPI) SharedMembers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string{}, m.sharedMembers...)
}

func (m *MockAPI) DriveSetLinkShare(ctx context.Context, token, entity string) error {
	if err := m.gate(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	if m.linkShares == nil {
		m.linkShares = map[string]string{}
	}
	m.linkShares[token] = entity
	name := m.driveNames[token]
	m.mu.Unlock()
	if strings.Contains(name, "noshare") {
		return &FeishuError{Code: 1063002, HTTPStatus: 403, Msg: "Permission denied"}
	}
	return nil
}

func (m *MockAPI) LinkShare(token string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.linkShares == nil {
		return ""
	}
	return m.linkShares[token]
}

// DriveMeta returns metadata for a Drive document in MockAPI.
func (m *MockAPI) DriveMeta(ctx context.Context, token string) (*DriveDocMeta, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	if token == "boxmockdenied" {
		return nil, &FeishuError{Code: 0, HTTPStatus: 403, Msg: "没有查看该云空间文件的权限"}
	}
	m.mu.RLock()
	name := m.driveNames[token]
	mod := m.driveModTimes[token]
	m.mu.RUnlock()
	if name == "" {
		name = "云空间文件-" + token + ".txt"
	}
	if mod == 0 {
		mod = time.Now().UnixMilli()
	}
	return &DriveDocMeta{
		OwnerID: func() string {
			if m.auth != nil {
				return m.auth.UserID()
			}
			return "ou_mock_owner"
		}(),
		DocToken:         token,
		DocType:          "file",
		Title:            name,
		LatestModifyTime: mod,
		URL:              "https://mock.feishu.cn/file/" + token,
	}, nil
}

// SetDriveFile updates the name, content, and modification timestamp for a mock drive file.
func (m *MockAPI) SetDriveFile(token, name string, content []byte, modTime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driveNames == nil {
		m.driveNames = map[string]string{}
	}
	if m.driveModTimes == nil {
		m.driveModTimes = map[string]int64{}
	}
	if m.driveFiles == nil {
		m.driveFiles = map[string][]byte{}
	}
	m.driveNames[token] = name
	m.driveFiles[token] = content
	m.driveModTimes[token] = modTime
}

// DriveDownload serves files whose token starts with "boxmock" (uploaded from
// the app or seeded); "boxmockdenied" simulates a missing permission.
func (m *MockAPI) DriveDownload(ctx context.Context, token string) (*Download, error) {
	if err := m.gate(ctx); err != nil {
		return nil, err
	}
	if token == "boxmockdenied" {
		return nil, &FeishuError{Code: 0, HTTPStatus: 403, Msg: "没有下载该云空间文件的权限"}
	}
	if !strings.HasPrefix(token, "boxmock") {
		return nil, &FeishuError{HTTPStatus: 404, Msg: "文件不存在或已被删除"}
	}
	m.mu.RLock()
	name := m.driveNames[token]
	customBytes, hasCustom := m.driveFiles[token]
	m.mu.RUnlock()
	if name == "" {
		name = "云空间文件-" + token + ".txt"
	}
	if hasCustom {
		return &Download{Body: io.NopCloser(bytes.NewReader(customBytes)), ContentType: "application/octet-stream", Filename: name}, nil
	}
	return &Download{Body: io.NopCloser(strings.NewReader("mock drive file " + token + "\n")), ContentType: "application/octet-stream", Filename: name}, nil
}

// SendBotCard mocks sending an interactive card to userOpenID.
func (m *MockAPI) SendBotCard(ctx context.Context, openID string, card any) error {
	if err := m.gate(ctx); err != nil {
		return err
	}
	return nil
}
