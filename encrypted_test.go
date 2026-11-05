package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTopSystemGuardrails(t *testing.T) {
	txt := TopSystemGuardrails("测试话题", "https://example.feishu.cn/docx/123")
	if !strings.Contains(txt, "呆呆鸟系统最高安全准则") {
		t.Fatalf("缺少顶级安全准则声明")
	}
	if !strings.Contains(txt, "全成果文档落盘") || !strings.Contains(txt, "群聊严格闭嘴禁言") || !strings.Contains(txt, "Plain Text 代码块") {
		t.Fatalf("核心约束缺失: %s", txt)
	}
	if strings.Contains(txt, "加密") {
		t.Fatalf("准则中严禁出现加密字眼")
	}
}

func TestFormatAndParseEncryptedDocContent(t *testing.T) {
	// 1. Format Turn 1
	turn1 := FormatTurnBlock(1, "帮我分析一下 Q3 营收数据", []string{"revenue.xlsx"}, "")
	if !strings.Contains(turn1, "Turn 1") || !strings.Contains(turn1, "帮我分析一下 Q3 营收数据") {
		t.Fatalf("FormatTurnBlock 格式异常: %s", turn1)
	}

	// Simulate Bot writing reply
	turn1Completed := strings.Replace(turn1, "*(呆呆鸟执行任务中，内容将实时落盘于此处...)*", "Q3 营收总计 1.2 亿元，同比增长 15%。", 1)
	turn1Completed = strings.Replace(turn1Completed, "- *(待呆呆鸟在任务完成后将生成的文件回填链接在此处)*", "- [分析图表.pdf](https://example.feishu.cn/file/f1)", 1)

	// 2. Format Turn 2 with history
	history := "#### Turn 1 [已固化]\n- 用户: 帮我分析营收\n- 呆呆鸟: 营收1.2亿\n\n"
	turn2 := FormatTurnBlock(2, "生成对应的 PPT 汇报", nil, history)

	fullDoc := TopSystemGuardrails("测试", "https://example.feishu.cn/docx/fld") + "\n" + turn1Completed + "\n" + turn2

	// 3. Parse doc
	parsed := ParseEncryptedDocContent(fullDoc)
	if len(parsed) != 2 {
		t.Fatalf("期望解析出 2 个轮次，实际解析出 %d", len(parsed))
	}

	// Verify Turn 1
	p1 := parsed[0]
	if p1.TurnNumber != 1 {
		t.Errorf("Turn 1 轮次编号错误: %d", p1.TurnNumber)
	}
	if p1.UserPrompt != "帮我分析一下 Q3 营收数据" {
		t.Errorf("Turn 1 用户输入解析错误: %q", p1.UserPrompt)
	}
	if !p1.IsCompleted || !strings.Contains(p1.BotReply, "Q3 营收总计 1.2 亿元") {
		t.Errorf("Turn 1 Bot 回复解析错误: %q (completed=%v)", p1.BotReply, p1.IsCompleted)
	}
	if len(p1.Artifacts) == 0 || !strings.Contains(p1.Artifacts[0], "分析图表.pdf") {
		t.Errorf("Turn 1 附件解析错误: %+v", p1.Artifacts)
	}

	// Verify Turn 2 (running)
	p2 := parsed[1]
	if p2.TurnNumber != 2 {
		t.Errorf("Turn 2 轮次编号错误: %d", p2.TurnNumber)
	}
	if p2.UserPrompt != "生成对应的 PPT 汇报" {
		t.Errorf("Turn 2 用户输入解析错误: %q", p2.UserPrompt)
	}
	if p2.IsCompleted {
		t.Errorf("Turn 2 尚未完成，不应为 is_completed")
	}
}

func TestBuildDocxTextBlocks(t *testing.T) {
	md := `# 标题一
## 标题二
### 标题三
> 引用警告

普通段落内容
---`
	blocks := BuildDocxTextBlocks(md)
	if len(blocks) < 5 {
		t.Fatalf("生成的 Block 数量不足: %d", len(blocks))
	}

	// Block types: H1 (3), H2 (4), H3 (5), Quote (17), Divider (22)
	foundH1 := false
	foundDivider := false
	for _, b := range blocks {
		bt, _ := b["block_type"].(int)
		if bt == 3 {
			foundH1 = true
		}
		if bt == 22 {
			foundDivider = true
		}
	}
	if !foundH1 || !foundDivider {
		t.Fatalf("未找到预期的 H1 或 Divider block")
	}
}

func TestEncryptedStore(t *testing.T) {
	dir := t.TempDir()
	store := NewEncryptedStore(dir)

	// Save thread
	thread := &EncryptedThread{
		ThreadID:     "thread_001",
		DocToken:     "docx_tok_1",
		DocURL:       "https://feishu.cn/docx/docx_tok_1",
		FolderToken:  "fld_tok_1",
		Title:        "安全审计话题",
		OwnerOpenID:  "ou_user_1",
		ContextLimit: 10,
	}
	if err := store.SaveThread(thread); err != nil {
		t.Fatalf("SaveThread 失败: %v", err)
	}

	// Retrieve thread
	th, ok := store.GetThread("thread_001")
	if !ok || th.DocToken != "docx_tok_1" {
		t.Fatalf("GetThread 检索失败: %+v", th)
	}

	// Update ContextLimit
	if err := store.UpdateContextLimit("thread_001", 20); err != nil {
		t.Fatalf("UpdateContextLimit 失败: %v", err)
	}
	th, _ = store.GetThread("thread_001")
	if th.ContextLimit != 20 {
		t.Errorf("ContextLimit 未更新: %d", th.ContextLimit)
	}

	// Add user & bot messages
	now := time.Now()
	_ = store.AddMessage(EncryptedMessage{
		ID:         "msg_u1",
		ThreadID:   "thread_001",
		TurnNumber: 1,
		Role:       "user",
		Content:    "Hello",
		IsFrozen:   true,
		Status:     "done",
		CreatedAt:  now,
	})
	_ = store.AddMessage(EncryptedMessage{
		ID:         "msg_b1",
		ThreadID:   "thread_001",
		TurnNumber: 1,
		Role:       "assistant",
		Content:    "Running...",
		IsFrozen:   false,
		Status:     "running",
		CreatedAt:  now,
	})

	msgs := store.GetMessages("thread_001")
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，实际: %d", len(msgs))
	}

	// Update bot reply to done and freeze
	store.UpdateAssistantTurn("thread_001", 1, "Completed reply", []string{"link1"}, "done", true)
	msgs = store.GetMessages("thread_001")
	if msgs[1].Status != "done" || !msgs[1].IsFrozen || msgs[1].Content != "Completed reply" {
		t.Errorf("UpdateAssistantTurn 异常: %+v", msgs[1])
	}

	if store.GetLatestTurnNumber("thread_001") != 1 {
		t.Errorf("GetLatestTurnNumber 异常: %d", store.GetLatestTurnNumber("thread_001"))
	}
}

func TestEncryptedServiceMockFlow(t *testing.T) {
	dir := t.TempDir()
	auth := NewAuth(realConfig("https://open.feishu.cn"), false, dir+"/auth.json")
	auth.tok = &tokenFile{User: UserInfo{OpenID: "ou_tester", Name: "测试员"}, ExpiresAt: time.Now().Add(time.Hour)}

	api := NewMockAPI(auth)
	cfg := Config{DataDir: dir, BaseURL: "https://open.feishu.cn"}
	svc := NewService(api, NewCache(dir+"/cache"), cfg, auth)

	ctx := context.Background()

	// 1. Ensure Doc
	thread, err := svc.EnsureEncryptedDoc(ctx, "omt_mock_001", "商业化方案分析")
	if err != nil {
		t.Fatalf("EnsureEncryptedDoc 失败: %v", err)
	}
	if !strings.HasPrefix(thread.DocToken, "docx_mock_") {
		t.Errorf("DocToken 非预期: %s", thread.DocToken)
	}

	// 2. Send Turn
	botMsg, err := svc.SendEncryptedTurn(ctx, "omt_mock_001", "请帮我制定第一阶段实施路线", nil, 10)
	if err != nil {
		t.Fatalf("SendEncryptedTurn 失败: %v", err)
	}
	if botMsg.TurnNumber != 1 || botMsg.Status != "running" {
		t.Errorf("新轮次状态非预期: %+v", botMsg)
	}

	// Verify the notification message sent into the topic:
	// MUST be plain text code_block without verbose prefix or explanatory text
	sentUUID := fmt.Sprintf("enc_turn_omt_mock_001_1")
	foundNotify, ok := api.sent[sentUUID]
	if !ok || foundNotify == nil {
		t.Fatalf("必须向话题发送通知，未在 api.sent 找到 uuid=%s", sentUUID)
	}
	if foundNotify.MsgType != "post" {
		t.Fatalf("通知消息类型必须为 post，实际为: %s", foundNotify.MsgType)
	}
	if !strings.Contains(foundNotify.Body.Content, "code_block") || !strings.Contains(foundNotify.Body.Content, thread.DocURL) {
		t.Fatalf("通知消息必须包含 code_block 与文档链接，实际为: %s", foundNotify.Body.Content)
	}
	if strings.Contains(foundNotify.Body.Content, "【🔒 加密会话任务通知】") || strings.Contains(foundNotify.Body.Content, "未开放权限") || strings.Contains(foundNotify.Body.Content, "加密") {
		t.Fatalf("严禁包含冗余或‘加密’等敏感解释性文本")
	}

	// 3. Get Thread View
	th, msgs, err := svc.GetEncryptedThread(ctx, "omt_mock_001")
	if err != nil {
		t.Fatalf("GetEncryptedThread 失败: %v", err)
	}
	if th.ThreadID != "omt_mock_001" || len(msgs) != 2 {
		t.Errorf("Thread 消息列表非预期: len=%d", len(msgs))
	}
}

func TestVerifyStrictPermissionsSnapshot(t *testing.T) {
	owner := "ou_owner123"
	bot := RedDaidainiaoBotOpenID

	// 1. Valid snapshot
	validSnap := &DrivePermissionSnapshot{
		LinkShare: "closed",
		Members: []DrivePermissionMember{
			{MemberType: "openid", ID: owner, Perm: "full_access"},
			{MemberType: "openid", ID: bot, Perm: "edit"},
		},
	}
	if err := VerifyStrictPermissionsSnapshot(validSnap, owner, bot); err != nil {
		t.Fatalf("正常白名单校验应通过，实际失败: %v", err)
	}

	// 2. Open link share violation
	leakedLink := &DrivePermissionSnapshot{
		LinkShare: "tenant_readable",
		Members:   validSnap.Members,
	}
	if err := VerifyStrictPermissionsSnapshot(leakedLink, owner, bot); err == nil {
		t.Fatalf("链接分享未关闭必须拦截")
	}

	// 3. Openchat violation (group chat permission)
	chatLeak := &DrivePermissionSnapshot{
		LinkShare: "closed",
		Members: []DrivePermissionMember{
			{MemberType: "openid", ID: owner, Perm: "full_access"},
			{MemberType: "openid", ID: bot, Perm: "edit"},
			{MemberType: "openchat", ID: "oc_group_123", Perm: "view"},
		},
	}
	if err := VerifyStrictPermissionsSnapshot(chatLeak, owner, bot); err == nil {
		t.Fatalf("群组权限必须严格拦截")
	}

	// 4. Third party unauthorized member violation
	thirdPartyLeak := &DrivePermissionSnapshot{
		LinkShare: "closed",
		Members: []DrivePermissionMember{
			{MemberType: "openid", ID: owner, Perm: "full_access"},
			{MemberType: "openid", ID: bot, Perm: "edit"},
			{MemberType: "openid", ID: "ou_stranger", Perm: "view"},
		},
	}
	if err := VerifyStrictPermissionsSnapshot(thirdPartyLeak, owner, bot); err == nil {
		t.Fatalf("第三方非白名单成员必须拦截")
	}
}

func TestLiveEncryptedDoc(t *testing.T) {
	configPath := os.Getenv("DAIDAINIAO_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("skipping live test without DAIDAINIAO_LIVE_CONFIG")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	dataDir := cfg.DataDir
	if v := os.Getenv("DAIDAINIAO_LIVE_DATA"); v != "" {
		dataDir = v
	}
	cfg.DataDir = dataDir
	auth := NewAuth(cfg, false, filepath.Join(cfg.DataDir, "token.json"))
	if !auth.LoggedIn() {
		t.Fatal("auth not logged in")
	}
	client := NewFeishuClient(cfg.BaseURL, cfg.AppID, cfg.AppSecret, auth, "auto")
	svc := NewService(client, NewCache(filepath.Join(cfg.DataDir, "cache")), cfg, auth)

	ctx := context.Background()
	thread, err := svc.EnsureEncryptedDoc(ctx, "omt_1989d661cf0f1bb7", "北京现代运维手册")
	if err != nil {
		t.Fatalf("EnsureEncryptedDoc failed: %v", err)
	}
	t.Logf("Created/Ensured encrypted doc: doc_token=%s url=%s folder=%s", thread.DocToken, thread.DocURL, thread.FolderURL)
}

func TestLiveDocxRawContent(t *testing.T) {
	configPath := os.Getenv("DAIDAINIAO_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("skipping live test without DAIDAINIAO_LIVE_CONFIG")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	dataDir := cfg.DataDir
	if v := os.Getenv("DAIDAINIAO_LIVE_DATA"); v != "" {
		dataDir = v
	}
	cfg.DataDir = dataDir
	auth := NewAuth(cfg, false, filepath.Join(cfg.DataDir, "token.json"))
	client := NewFeishuClient(cfg.BaseURL, cfg.AppID, cfg.AppSecret, auth, "auto")

	ctx := context.Background()
	raw, err := client.GetDocxRawContent(ctx, "GxV1d9tb4oQeiwxHhNpc0xsvn5b")
	if err != nil {
		t.Fatalf("GetDocxRawContent failed: %v", err)
	}
	t.Logf("RAW CONTENT LENGTH: %d", len(raw))
	t.Logf("RAW CONTENT:\n%s", raw)

	parsed := ParseEncryptedDocContent(raw)
	t.Logf("PARSED TURNS COUNT: %d", len(parsed))
	for _, p := range parsed {
		t.Logf("Parsed turn %d: completed=%v, prompt=%q, reply=%q", p.TurnNumber, p.IsCompleted, p.UserPrompt, p.BotReply)
	}
}
