package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const EncryptedDrivePrivateRootName = "网页呆呆鸟-工作文档"

// EnsureEncryptedDoc ensures a 1:1 Feishu Docx document exists for the given thread.
func (s *Service) EnsureEncryptedDoc(ctx context.Context, threadID, title string) (*EncryptedThread, error) {
	if s.encryptedStore == nil {
		s.encryptedStore = NewEncryptedStore(s.cfg.DataDir)
	}

	limit := -1 // 默认仅本轮 (极致隐私/无历史)
	if existing, ok := s.encryptedStore.GetThread(threadID); ok {
		if existing.DocToken != "" {
			return existing, nil
		}
		if existing.ContextLimit != 0 {
			limit = existing.ContextLimit
		}
	}

	if err := s.requireLogin(); err != nil {
		return nil, err
	}

	ownerID := s.auth.UserID()
	ownerName := "当前用户"
	if st := s.auth.Status(); st.User != nil && st.User.Name != "" {
		ownerName = st.User.Name
	}

	if title == "" {
		if raws, err := s.threadMessagesForReply(ctx, threadID); err == nil && len(raws) > 0 {
			root := raws[0]
			for _, m := range raws {
				if isRoot(m) {
					root = m
					break
				}
			}
			parsed := ParseContent(root.MsgType, root.Body.Content, root.Mentions)
			t := firstLine(parsed.Plain, 60)
			if t != "" {
				title = t
			}
		}
	}
	title = strings.ReplaceAll(title, "/", "_")
	title = strings.ReplaceAll(title, "\\", "_")
	title = strings.TrimSpace(title)
	if title == "" {
		title = fmt.Sprintf("会话_%s", threadID)
	}

	client, isReal := s.api.(*FeishuClient)
	if isReal {
		// 1. Ensure private dedicated folder
		fToken, _, err := client.ensureEncryptedTopicFolder(ctx, title)
		if err != nil {
			return nil, fmt.Errorf("创建私密会话文件夹失败：%w", err)
		}
		fURL, _ := client.FolderURL(ctx, fToken)

		// 2. Create Docx
		docx, err := client.CreateDocx(ctx, fToken, title+" · 协同文档")
		if err != nil {
			return nil, fmt.Errorf("创建协同文档失败：%w", err)
		}
		docURL, _ := client.DocxURL(ctx, docx.DocumentID)

		// 3. Ensure strictly private (no link share, grant bot edit)
		if err := client.EnsureDocxStrictlyPrivate(ctx, docx.DocumentID, RedDaidainiaoBotOpenID); err != nil {
			return nil, fmt.Errorf("设置文档严格私密权限失败：%w", err)
		}

		// 4. Verify ACL
		if err := client.VerifyStrictEncryptedPermissions(ctx, docx.DocumentID, ownerID, RedDaidainiaoBotOpenID); err != nil {
			return nil, fmt.Errorf("安全校验未通过：%w", err)
		}

		// 5. Append Top Guardrails
		topMarkdown := TopSystemGuardrails(threadID, title, ownerName, fURL)
		blocks := BuildDocxTextBlocks(topMarkdown)
		if err := client.AppendDocxChildren(ctx, docx.DocumentID, "", blocks, -1); err != nil {
			return nil, fmt.Errorf("写入会话初始安全准则失败：%w", err)
		}

		thread := &EncryptedThread{
			ThreadID:       threadID,
			DocToken:       docx.DocumentID,
			DocURL:         docURL,
			FolderToken:    fToken,
			FolderURL:      fURL,
			Title:          title,
			OwnerOpenID:    ownerID,
			ContextLimit:   limit,
			LatestRevision: docx.RevisionID,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}
		if err := s.encryptedStore.SaveThread(thread); err != nil {
			return nil, err
		}
		return thread, nil
	}

	// Mock mode fallback for local test/mock environments
	mockDocToken := "docx_mock_" + threadID
	mockFolderToken := "fld_mock_" + threadID
	thread := &EncryptedThread{
		ThreadID:       threadID,
		DocToken:       mockDocToken,
		DocURL:         "https://example.feishu.cn/docx/" + mockDocToken,
		FolderToken:    mockFolderToken,
		FolderURL:      "https://example.feishu.cn/drive/folder/" + mockFolderToken,
		Title:          title,
		OwnerOpenID:    ownerID,
		ContextLimit:   limit,
		LatestRevision: 1,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := s.encryptedStore.SaveThread(thread); err != nil {
		return nil, err
	}
	return thread, nil
}

// SendEncryptedTurn appends a new conversation turn to the Docx document and notifies via Plain Text IM.
func (s *Service) SendEncryptedTurn(ctx context.Context, threadID, prompt string, attachments []string, contextLimit int) (*EncryptedMessage, error) {
	if s.encryptedStore == nil {
		s.encryptedStore = NewEncryptedStore(s.cfg.DataDir)
	}

	prompt = strings.TrimSpace(prompt)
	if prompt == "" && len(attachments) == 0 {
		return nil, fmt.Errorf("回复内容不能为空")
	}

	thread, err := s.EnsureEncryptedDoc(ctx, threadID, "")
	if err != nil {
		return nil, fmt.Errorf("初始化会话文档失败：%w", err)
	}

	if contextLimit == 0 {
		if thread.ContextLimit != 0 {
			contextLimit = thread.ContextLimit
		} else {
			contextLimit = -1
		}
	}

	// Calculate turn number
	turnNumber := s.encryptedStore.GetLatestTurnNumber(threadID) + 1

	// Assemble history snippet if requested
	historySnippet := ""
	if contextLimit != -1 {
		allMsgs := s.encryptedStore.GetMessages(threadID)
		historySnippet = s.buildHistorySnippet(allMsgs, contextLimit)
	}

	turnMarkdown := FormatTurnBlock(turnNumber, prompt, attachments, historySnippet)

	// Append to Docx if real Feishu client
	client, isReal := s.api.(*FeishuClient)
	if isReal {
		blocks := BuildDocxTextBlocks(turnMarkdown)
		if err := client.AppendDocxChildren(ctx, thread.DocToken, "", blocks, -1); err != nil {
			return nil, fmt.Errorf("向飞书文档写入新轮次失败：%w", err)
		}
	}

	now := time.Now()
	userMsgID := fmt.Sprintf("msg_%s_turn%d_user", threadID, turnNumber)
	userMsg := EncryptedMessage{
		ID:          userMsgID,
		ThreadID:    threadID,
		TurnNumber:  turnNumber,
		Role:        "user",
		Content:     prompt,
		Attachments: attachments,
		IsFrozen:    true,
		Status:      "done",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = s.encryptedStore.AddMessage(userMsg)

	botMsgID := fmt.Sprintf("msg_%s_turn%d_bot", threadID, turnNumber)
	botMsg := EncryptedMessage{
		ID:         botMsgID,
		ThreadID:   threadID,
		TurnNumber: turnNumber,
		Role:       "assistant",
		Content:    "*(呆呆鸟正在云文档中输出，完成后将自动同步...)*",
		IsFrozen:   false,
		Status:     "running",
		CreatedAt:  now.Add(100 * time.Millisecond),
		UpdatedAt:  now.Add(100 * time.Millisecond),
	}
	_ = s.encryptedStore.AddMessage(botMsg)

	// Send Plain Text Code Block Notification into Feishu IM Topic
	// Notice: Must be a pure post message with tag=="code_block", never rich text, card, or plain URL,
	// to prevent Feishu client from automatically generating preview cards and granting chat read access.
	// No verbose prefix or explanation is sent, only the plain text code block containing the document URL.
	postContent := map[string]any{
		"zh_cn": map[string]any{
			"title": "",
			"content": [][]map[string]any{
				{
					{"tag": "code_block", "text": thread.DocURL},
				},
			},
		},
	}

	contentJSON, _ := json.Marshal(postContent)
	rootID := threadID
	if raws, err := s.threadMessagesForReply(ctx, threadID); err == nil && len(raws) > 0 {
		if r := rootMessageID(raws); r != "" {
			rootID = r
		}
	}

	_, _ = s.api.ReplyMessage(ctx, rootID, "post", string(contentJSON), fmt.Sprintf("enc_turn_%s_%d", threadID, turnNumber), true)

	return &botMsg, nil
}

// SyncEncryptedDoc pulls raw content from the Docx document if revision has changed, updating the local store.
func (s *Service) SyncEncryptedDoc(ctx context.Context, threadID string) (*EncryptedThread, []EncryptedMessage, error) {
	if s.encryptedStore == nil {
		s.encryptedStore = NewEncryptedStore(s.cfg.DataDir)
	}

	thread, ok := s.encryptedStore.GetThread(threadID)
	if !ok {
		return nil, nil, fmt.Errorf("加密会话不存在")
	}

	client, isReal := s.api.(*FeishuClient)
	if isReal {
		meta, err := client.GetDocxMeta(ctx, thread.DocToken)
		if err == nil && meta != nil {
			// Check if revision bumped or if there are unfinished turns
			hasRunning := false
			for _, m := range s.encryptedStore.GetMessages(threadID) {
				if m.Status == "running" {
					hasRunning = true
					break
				}
			}

			if meta.RevisionID > thread.LatestRevision || hasRunning {
				raw, err := client.GetDocxRawContent(ctx, thread.DocToken)
				if err == nil && raw != "" {
					parsedTurns := ParseEncryptedDocContent(raw)
					for _, pt := range parsedTurns {
						if pt.IsCompleted {
							s.encryptedStore.UpdateAssistantTurn(threadID, pt.TurnNumber, pt.BotReply, pt.Artifacts, "done", true)
						} else if pt.BotReply != "" {
							s.encryptedStore.UpdateAssistantTurn(threadID, pt.TurnNumber, pt.BotReply, pt.Artifacts, "running", false)
						}
					}
					s.encryptedStore.UpdateLatestRevision(threadID, meta.RevisionID)
					thread.LatestRevision = meta.RevisionID
				}
			}
		}
	}

	msgs := s.encryptedStore.GetMessages(threadID)
	return thread, msgs, nil
}

// GetEncryptedThread returns the thread view with all parsed turns.
func (s *Service) GetEncryptedThread(ctx context.Context, threadID string) (*EncryptedThread, []EncryptedMessage, error) {
	return s.SyncEncryptedDoc(ctx, threadID)
}

// AuditEncryptedPermissions audits the Feishu Docx document permissions against strict zero-trust rules.
func (s *Service) AuditEncryptedPermissions(ctx context.Context, threadID string) (*DrivePermissionSnapshot, error) {
	if s.encryptedStore == nil {
		s.encryptedStore = NewEncryptedStore(s.cfg.DataDir)
	}
	thread, ok := s.encryptedStore.GetThread(threadID)
	if !ok {
		return nil, fmt.Errorf("会话不存在")
	}

	client, isReal := s.api.(*FeishuClient)
	if !isReal {
		return &DrivePermissionSnapshot{
			LinkShare:    "closed",
			ShareEntity:  "same_tenant",
			ManageEntity: "collaborator_full_access",
			Members: []DrivePermissionMember{
				{ID: s.auth.UserID(), MemberType: "openid", Perm: "full_access"},
				{ID: RedDaidainiaoBotOpenID, MemberType: "openid", Perm: "edit"},
			},
		}, nil
	}

	if err := client.VerifyStrictEncryptedPermissions(ctx, thread.DocToken, thread.OwnerOpenID, RedDaidainiaoBotOpenID); err != nil {
		return nil, err
	}

	return client.DrivePermissions(ctx, thread.DocToken, "docx")
}

// Helper to format past turns into a compact markdown summary
func (s *Service) buildHistorySnippet(allMsgs []EncryptedMessage, limit int) string {
	if limit <= 0 && limit != 0 {
		return ""
	}

	// Pair into turns
	type turnPair struct {
		turnNum int
		user    string
		bot     string
		time    string
	}
	turnsMap := make(map[int]*turnPair)
	for _, m := range allMsgs {
		if _, ok := turnsMap[m.TurnNumber]; !ok {
			turnsMap[m.TurnNumber] = &turnPair{turnNum: m.TurnNumber}
		}
		p := turnsMap[m.TurnNumber]
		if m.Role == "user" {
			p.user = m.Content
			p.time = m.CreatedAt.Format("2006-01-02 15:04:05")
		} else if m.Role == "assistant" && m.Status == "done" {
			p.bot = m.Content
		}
	}

	var sortedTurns []*turnPair
	for _, p := range turnsMap {
		if p.user != "" && p.bot != "" {
			sortedTurns = append(sortedTurns, p)
		}
	}
	if len(sortedTurns) == 0 {
		return ""
	}

	// Sort ascending by turn number
	for i := 0; i < len(sortedTurns)-1; i++ {
		for j := i + 1; j < len(sortedTurns); j++ {
			if sortedTurns[i].turnNum > sortedTurns[j].turnNum {
				sortedTurns[i], sortedTurns[j] = sortedTurns[j], sortedTurns[i]
			}
		}
	}

	if limit > 0 && len(sortedTurns) > limit {
		sortedTurns = sortedTurns[len(sortedTurns)-limit:]
	}

	var sb strings.Builder
	for _, t := range sortedTurns {
		sb.WriteString(fmt.Sprintf("#### Turn %d | %s [已固化]\n", t.turnNum, t.time))
		sb.WriteString(fmt.Sprintf("- **用户**: %s\n", t.user))
		// Truncate long bot replies in history to save space
		botSnippet := t.bot
		if len([]rune(botSnippet)) > 200 {
			botSnippet = string([]rune(botSnippet)[:200]) + "..."
		}
		sb.WriteString(fmt.Sprintf("- **呆呆鸟**: %s\n\n", botSnippet))
	}

	return sb.String()
}
