package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in only after the user authorizes messages in the named topic.
// Uses a synthetic file, cleans its message and file, and never sends to another chat.
func TestLivePrivateMessage(t *testing.T) {
	root, chat := os.Getenv("DAIDAINIAO_LIVE_MESSAGE_ROOT"), os.Getenv("DAIDAINIAO_LIVE_MESSAGE_CHAT")
	configPath := os.Getenv("DAIDAINIAO_LIVE_CONFIG")
	if root == "" || chat == "" || configPath == "" {
		t.Skip("requires explicit authorized topic, chat and configuration")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Normalize()
	dataDir := os.Getenv("DAIDAINIAO_LIVE_DATA")
	if dataDir == "" {
		dataDir = cfg.DataDir
	}
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(filepath.Dir(configPath), dataDir)
	}
	auth := NewAuth(cfg, false, filepath.Join(dataDir, "token.json"))
	if auth.tok == nil || time.Until(auth.tok.ExpiresAt) < 3*time.Minute {
		t.Fatal("existing login must remain valid for at least three minutes")
	}
	client := NewFeishuClient(cfg.BaseURL, cfg.AppID, cfg.AppSecret, auth, cfg.ResourceAuth)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	items, err := client.GetMessage(ctx, root)
	if err != nil || len(items) == 0 || items[0].ChatID != chat {
		t.Fatalf("authorized target verification failed: %v", err)
	}
	cfg.DataDir = t.TempDir()
	svc := NewService(client, NewCache(filepath.Join(cfg.DataDir, "cache")), cfg, auth)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	code := "DODO_PRIVATE_VERIFY_" + stamp
	topic := "私密消息验收_" + stamp
	st, err := svc.Stage("私密权限验收.txt", strings.NewReader("verification_code="+code+"\nSynthetic permission verification file. No business data.\n"))
	if err != nil {
		t.Fatal(err)
	}
	var token, sentID string
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if sentID != "" {
			if e := client.callReq(cleanup, idUser, http.MethodDelete, "/im/v1/messages/"+url.PathEscape(sentID), nil, nil, true, nil); e != nil {
				t.Errorf("test message cleanup: %v", e)
			}
		}
		if token != "" {
			if e := client.callReq(cleanup, idUser, http.MethodDelete, "/drive/v1/files/"+url.PathEscape(token), url.Values{"type": {"file"}}, nil, true, nil); e != nil {
				t.Errorf("test file cleanup: %v", e)
			}
		}
		if folder, _, e := client.ensurePrivateTopicFolder(cleanup, topic); e == nil {
			if page, e := client.DriveList(cleanup, folder, ""); e == nil && len(page.Files) == 0 && !page.HasMore {
				if e = client.callReq(cleanup, idUser, http.MethodDelete, "/drive/v1/files/"+url.PathEscape(folder), url.Values{"type": {"folder"}}, nil, true, nil); e != nil {
					t.Errorf("test folder cleanup: %v", e)
				}
			}
		}
	}()
	_, data, err := svc.loadStaged(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	part, err := svc.uploadOne(ctx, st, data, chat, topic, svc.resolveBotIDs(ctx, composeTarget{}, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	token = part.Key
	t.Log("Synthetic private file ready; sending only to the authorized test topic")
	instruction := "常规附件读取验收：本次明确允许并要求使用当前话题中已授权的当前用户身份读取此专用测试附件，只回复文件内 verification_code。不要转发链接，不要添加或修改协作者。附件无业务内容。"
	if os.Getenv("DAIDAINIAO_LIVE_APP_ONLY") == "1" {
		instruction = "应用权限验收：本次仅允许使用呆呆鸟应用自身身份读取专用测试附件，不允许使用用户身份。只回复文件内 verification_code。不要转发链接，不要添加或修改协作者。附件无业务内容。"
	}
	msg := assemble(instruction, nil, []*sendPart{part})[0]
	if os.Getenv("DAIDAINIAO_LIVE_MESSAGE_FORMAT") == "code_block" {
		body, _ := json.Marshal(map[string]any{"zh_cn": map[string]any{"title": "", "content": [][]map[string]any{
			{{"tag": "text", "text": instruction}},
			{{"tag": "text", "text": part.Name}},
			{{"tag": "code_block", "text": part.URL}},
		}}})
		msg = outMsg{Type: "post", Content: string(body)}
	}
	var resp *RawMessage
	if os.Getenv("DAIDAINIAO_LIVE_MESSAGE_FORMAT") == "code_block" {
		resp, err = client.ReplyMessage(ctx, root, msg.Type, msg.Content, "private-check-"+stamp, true)
	} else {
		var result *ComposeResult
		result, err = svc.compose(ctx, composeTarget{chatID: chat, rootID: root, threadID: items[0].ThreadID, topicFolder: topic}, instruction, nil, "private-check-"+stamp, []string{st.ID})
		if err == nil {
			if len(result.raws) != 1 {
				t.Fatal("expected exactly one committed test message")
			}
			resp = &result.raws[0]
			sentID = resp.MessageID
			if len(result.Warnings) > 0 {
				t.Fatalf("production compose warnings: %v", result.Warnings)
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	sentID = resp.MessageID
	t.Log("Test message committed; verifying permissions and target application read")
	if sentID == "" {
		t.Fatal("missing message id")
	}
	thread := resp.ThreadID
	if thread == "" {
		thread = items[0].ThreadID
	}
	readConfirmed := false
	checks := 0
	for i := 0; i < 13; i++ {
		if i > 0 {
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		perms, e := client.DrivePermissions(ctx, token, "file")
		if e != nil {
			t.Fatal(e)
		}
		if e = verifyPrivatePermissions(perms, auth.UserID(), part.Staged.DriveBotMember.MemberType+":"+part.Staged.DriveBotMember.ID, ""); e != nil {
			t.Fatalf("after-send permissions at check %d: %v", i, e)
		}
		checks++
		if thread != "" {
			page, e := client.ListMessages(ctx, "thread", thread, "", true)
			if e != nil {
				t.Fatal(e)
			}
			for _, m := range page.Items {
				if m.Sender.SenderType == "app" && (m.Sender.ID == part.Staged.DriveBotMember.ID || m.Sender.ID == RedDaidainiaoBotOpenID) && strings.Contains(m.Body.Content, code) {
					readConfirmed = true
				}
			}
		}
		if i >= 3 && (readConfirmed || os.Getenv("DAIDAINIAO_LIVE_REQUIRE_BOT_READ") != "1") {
			break
		}
	}
	t.Logf("POST_SEND_PRIVACY_PASS checks=%d bot_file_content_confirmed=%v", checks, readConfirmed)
	if os.Getenv("DAIDAINIAO_LIVE_REQUIRE_BOT_READ") == "1" && !readConfirmed {
		t.Fatal("target bot has not returned the file-only verification code")
	}
}
