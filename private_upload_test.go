package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type privacyAPI struct {
	*MockAPI
	uploads            atomic.Int32
	sends              atomic.Int32
	failClose          bool
	failReadAfterGrant bool
	extraMember        bool
	wrongOwner         bool
	echoOpenID         bool
	unsafeLinkSent     atomic.Bool
	autoGrantLinks     bool
	extraAfterSend     bool
	failAfterSend      bool
	failSend           bool
}

func (p *privacyAPI) DriveUpload(ctx context.Context, n string, b []byte) (string, error) {
	p.uploads.Add(1)
	return p.MockAPI.DriveUpload(ctx, n, b)
}
func (p *privacyAPI) DriveClosePrivate(ctx context.Context, t, k string) error {
	if p.failClose {
		return fmt.Errorf("close failed")
	}
	return p.MockAPI.DriveClosePrivate(ctx, t, k)
}
func (p *privacyAPI) DrivePermissions(ctx context.Context, t, k string) (*DrivePermissionSnapshot, error) {
	if p.failAfterSend && p.sends.Load() > 0 {
		return nil, fmt.Errorf("post-send permission read interrupted")
	}
	if p.failReadAfterGrant && len(p.SharedMembers()) > 0 {
		return nil, fmt.Errorf("readback interrupted")
	}
	v, err := p.MockAPI.DrivePermissions(ctx, t, k)
	if err == nil && (p.extraMember || p.unsafeLinkSent.Load() || (p.extraAfterSend && p.sends.Load() > 0)) {
		v.Members = append(v.Members, DrivePermissionMember{ID: "oc_other", MemberType: "openchat", Perm: "view", Type: "chat"})
	}
	return v, err
}
func (p *privacyAPI) DriveShareMember(ctx context.Context, t, typ, id, perm string) (*DrivePermissionMember, error) {
	v, err := p.MockAPI.DriveShareMember(ctx, t, typ, id, perm)
	if err == nil && p.echoOpenID {
		return &DrivePermissionMember{ID: id, MemberType: typ, Perm: perm, Type: "user"}, nil
	}
	return v, err
}
func (p *privacyAPI) DriveMeta(ctx context.Context, t string) (*DriveDocMeta, error) {
	m, e := p.MockAPI.DriveMeta(ctx, t)
	if e == nil && p.wrongOwner {
		m.OwnerID = "ou_other"
	}
	return m, e
}
func (p *privacyAPI) ReplyMessage(ctx context.Context, id, typ, content, uuid string, inThread bool) (*RawMessage, error) {
	if p.failSend {
		return nil, fmt.Errorf("send blocked")
	}
	p.sends.Add(1)
	p.simulateLinkAuthorization(typ, content)
	return p.MockAPI.ReplyMessage(ctx, id, typ, content, uuid, inThread)
}
func (p *privacyAPI) SendMessage(ctx context.Context, id, typ, content, uuid string) (*RawMessage, error) {
	if p.failSend {
		return nil, fmt.Errorf("send blocked")
	}
	p.sends.Add(1)
	p.simulateLinkAuthorization(typ, content)
	return p.MockAPI.SendMessage(ctx, id, typ, content, uuid)
}

// Mirror the platform behavior that the original pre-send-only tests missed.
func (p *privacyAPI) simulateLinkAuthorization(typ, content string) {
	if !p.autoGrantLinks {
		return
	}
	var v any
	if json.Unmarshal([]byte(content), &v) != nil {
		return
	}
	var visit func(any)
	visit = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["tag"] == "code_block" {
				return
			}
			for _, val := range n {
				visit(val)
			}
		case []any:
			for _, val := range n {
				visit(val)
			}
		case string:
			for _, u := range urlRe.FindAllString(n, -1) {
				if isDocURL(u) {
					p.unsafeLinkSent.Store(true)
				}
			}
		}
	}
	visit(v)
}

func TestPrivateMessageAvoidsAutomaticChatGrant(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			_, svc := newTestServer(t)
			p := &privacyAPI{MockAPI: NewMockAPI(svc.auth), autoGrantLinks: true}
			svc.api = p
			st, err := svc.Stage("private.txt", strings.NewReader("private fixture"))
			if err != nil {
				t.Fatal(err)
			}
			target := composeTarget{chatID: "oc_mock_dev"}
			if reply {
				target.rootID = "om_mock_b01"
			}
			result, err := svc.compose(context.Background(), target, "请读取附件，另参考 https://x.feishu.cn/docx/reference", nil, "private-message", []string{st.ID})
			if err != nil || len(result.Messages) != 1 || len(result.Warnings) != 0 {
				t.Fatalf("compose: %+v %v", result, err)
			}
			if p.unsafeLinkSent.Load() {
				t.Fatal("normal cloud link would auto-authorize the chat")
			}
			parsed := ParseContent(result.raws[0].MsgType, result.raws[0].Body.Content, nil)
			if len(parsed.Attachments) != 2 {
				t.Fatalf("literal cloud links missing from file discovery: %+v", parsed.Attachments)
			}
		})
	}
}

func TestPrivatePostSendReadbackFailureDoesNotResend(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			_, svc := newTestServer(t)
			p := &privacyAPI{MockAPI: NewMockAPI(svc.auth), extraAfterSend: extra, failAfterSend: !extra}
			svc.api = p
			st, err := svc.Stage("private.txt", strings.NewReader("private fixture"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.compose(context.Background(), composeTarget{chatID: "oc_mock_dev"}, "private", nil, "private-post-send", []string{st.ID})
			if err != nil || len(result.Messages) != 1 || len(result.Failed) != 0 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "消息已发送") {
				t.Fatalf("committed message incorrectly reported: %+v %v", result, err)
			}
			if p.sends.Load() != 1 {
				t.Fatal("message resent after permission readback failure")
			}
		})
	}
}

func TestPrivateUploadRetriesReuseAndVerifyApplicationIdentity(t *testing.T) {
	for _, afterGrant := range []bool{false, true} {
		t.Run(fmt.Sprint(afterGrant), func(t *testing.T) {
			_, svc := newTestServer(t)
			p := &privacyAPI{MockAPI: NewMockAPI(svc.auth), failClose: !afterGrant, failReadAfterGrant: afterGrant, echoOpenID: true}
			svc.api = p
			st, err := svc.Stage("private.txt", strings.NewReader("private test"))
			if err != nil {
				t.Fatal(err)
			}
			tgt := composeTarget{chatID: "oc_mock_dev", rootID: "om_mock_b01"}
			_, err = svc.compose(context.Background(), tgt, "keep draft", nil, "private-retry", []string{st.ID})
			if err == nil || p.sends.Load() != 0 {
				t.Fatal("permission failure sent a message")
			}
			saved, _, err := svc.loadStaged(st.ID)
			if err != nil || saved.DriveToken == "" {
				t.Fatal("retry lost the uploaded token")
			}
			p.failClose, p.failReadAfterGrant = false, false
			res, err := svc.compose(context.Background(), tgt, "keep draft", nil, "private-retry", []string{st.ID})
			if err != nil || res == nil {
				t.Fatalf("retry: %v", err)
			}
			if p.uploads.Load() != 1 || p.sends.Load() != 1 {
				t.Fatalf("retry uploaded/sent twice: %d %d", p.uploads.Load(), p.sends.Load())
			}
			if len(p.SharedChats()) != 0 || len(p.SharedMembers()) != 1 {
				t.Fatal("grant exceeded the target application")
			}
		})
	}
}
func TestPrivateUploadRejectsExtraCollaboratorsWrongOwnerAndMultipleTargets(t *testing.T) {
	for _, scenario := range []string{"chat", "owner", "multiple"} {
		t.Run(scenario, func(t *testing.T) {
			_, svc := newTestServer(t)
			p := &privacyAPI{MockAPI: NewMockAPI(svc.auth), extraMember: scenario == "chat", wrongOwner: scenario == "owner"}
			svc.api = p
			if scenario == "multiple" {
				svc.cfg.BotOpenIDs = []string{"ou_a", "ou_b"}
			}
			st, err := svc.Stage("private.txt", strings.NewReader("private"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.compose(context.Background(), composeTarget{chatID: "oc_mock_dev", rootID: "om_mock_b01"}, "private", nil, "private-reject", []string{st.ID})
			if err == nil || p.sends.Load() != 0 {
				t.Fatalf("%s must stop sending", scenario)
			}
			if len(p.SharedChats()) != 0 {
				t.Fatal("group grant made")
			}
		})
	}
}

func TestFeishuPrivatePermissionProtocol(t *testing.T) {
	mux := http.NewServeMux()
	var patched, readMembers bool
	ok := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}
	mux.HandleFunc("/open-apis/drive/v2/permissions/private/public", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-user" || r.URL.Query().Get("type") != "file" {
			t.Error("wrong identity or type")
		}
		if r.Method == http.MethodPatch {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["link_share_entity"] != "closed" || body["manage_collaborator_entity"] != "collaborator_full_access" {
				t.Errorf("unsafe setting: %v", body)
			}
			patched = true
		}
		ok(w, map[string]any{"permission_public": map[string]string{"link_share_entity": "closed", "share_entity": "same_tenant", "manage_collaborator_entity": "collaborator_full_access"}})
	})
	mux.HandleFunc("/open-apis/drive/v1/permissions/private/members", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("fields") != "type" {
			t.Error("must request collaborator types")
		}
		readMembers = true
		ok(w, map[string]any{"items": []DrivePermissionMember{{MemberType: "openid", ID: "ou_owner", Perm: "full_access", Type: "user"}, {MemberType: "appid", ID: "cli_target", Perm: "view", Type: "app"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := defaultConfig()
	cfg.AppID = "cli_test"
	client := NewFeishuClient(srv.URL, "cli_test", "secret", loggedInAuth(t, cfg, "private-user", "r1", time.Hour), "auto")
	client.gap = 0
	if err := client.DriveClosePrivate(context.Background(), "private", "file"); err != nil {
		t.Fatal(err)
	}
	p, err := client.DrivePermissions(context.Background(), "private", "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivatePermissions(p, "ou_owner", "appid:cli_target", ""); err != nil {
		t.Fatal(err)
	}
	if !patched || !readMembers {
		t.Fatal("permission writes were not read back")
	}
}

func TestUnencryptedAttachmentGrantsOnlyCurrentChat(t *testing.T) {
	_, svc := newTestServer(t)
	p := &privacyAPI{MockAPI: NewMockAPI(svc.auth), failSend: true}
	svc.api = p
	st, err := svc.Stage("share.txt", strings.NewReader("hello group"))
	if err != nil {
		t.Fatal(err)
	}
	tgt := composeTarget{chatID: "oc_mock_dev", rootID: "om_mock_b01", shareWithChat: true}
	if _, err = svc.compose(context.Background(), tgt, "给群看", nil, "share-once", []string{st.ID}); err == nil || p.sends.Load() != 0 {
		t.Fatal("send should stop before the message")
	}
	saved, _, err := svc.loadStaged(st.ID)
	if err != nil || saved.DriveShareChat != "oc_mock_dev" || saved.DriveToken == "" {
		t.Fatalf("grant not retained: %+v %v", saved, err)
	}
	if chats := p.SharedChats(); len(chats) != 1 || chats[0] != "oc_mock_dev" || len(p.SharedMembers()) != 1 {
		t.Fatalf("grants chats=%v members=%v", p.SharedChats(), p.SharedMembers())
	}
	tgt.shareWithChat = false
	if _, err = svc.compose(context.Background(), tgt, "改回加密", nil, "share-lock", []string{st.ID}); err == nil || p.sends.Load() != 0 {
		t.Fatal("encrypted retry must stop after the chat grant")
	}
	p.failSend = false
	tgt.shareWithChat = true
	res, err := svc.compose(context.Background(), tgt, "给群看", nil, "share-send", []string{st.ID})
	if err != nil || res == nil || len(res.Messages) != 1 || len(res.Warnings) != 0 {
		t.Fatalf("resend: %+v %v", res, err)
	}
	if p.uploads.Load() != 1 || p.sends.Load() != 1 {
		t.Fatalf("retry uploaded or sent extra: uploads=%d sends=%d", p.uploads.Load(), p.sends.Load())
	}
	if !strings.Contains(res.Messages[0].Plain, "本群可阅读") || !strings.Contains(res.raws[0].Body.Content, `"tag":"code_block"`) {
		t.Fatalf("shared file must stay a literal link: %s", res.Messages[0].Plain)
	}
	var token string
	p.mu.RLock()
	for tok := range p.linkShares {
		token = tok
	}
	closed := p.linkShares[token]
	p.mu.RUnlock()
	if token == "" || closed != "closed" {
		t.Fatal("link share opened")
	}
}

func TestReplyEncryptFalseAndQueue(t *testing.T) {
	srv, svc := newTestServer(t)
	h := srv.Handler()
	f := stage(t, h, "群附件.txt", []byte("group readable"))["upload_id"].(string)
	rec, out := jsonReq(t, h, "POST", "/api/topics/omt_mock_002/reply", `{"text":"给群看","attachments":["`+f+`"],"encrypt":false}`)
	if rec.Code != 200 {
		t.Fatalf("reply: %d %v", rec.Code, out)
	}
	ma := svc.api.(*MockAPI)
	if chats := ma.SharedChats(); len(chats) != 1 || chats[0] != "oc_mock_dev" || len(ma.SharedMembers()) != 1 {
		t.Fatalf("grants chats=%v members=%v", ma.SharedChats(), ma.SharedMembers())
	}
	msgs := out["messages"].([]any)
	if !strings.Contains(msgs[0].(map[string]any)["plain"].(string), "本群可阅读") {
		t.Fatalf("plain: %v", msgs[0])
	}

	st, err := svc.Stage("q.txt", strings.NewReader("queued"))
	if err != nil {
		t.Fatal(err)
	}
	off := false
	req := ReplyRequest{Text: "排队给群", Attachments: []string{st.ID}, Encrypt: &off}
	atts, err := svc.validateAndLoadAttachments(req.Attachments)
	if err != nil {
		t.Fatal(err)
	}
	item := svc.enqueueReply("omt_mock_002", composeTarget{chatID: "oc_mock_dev", rootID: "om_mock_b01", threadID: "omt_mock_002"}, req, atts)
	if item.Encrypt == nil || *item.Encrypt {
		t.Fatal("queue dropped encrypt=false")
	}
	raw, err := os.ReadFile(svc.queueFilePath())
	if err != nil || !strings.Contains(string(raw), `"encrypt": false`) {
		t.Fatalf("queue file: %v %s", err, raw)
	}
	res, err := svc.dispatchQueuedItem(context.Background(), "omt_mock_002", item)
	if err != nil || len(res.Messages) != 1 || !strings.Contains(res.Messages[0].Plain, "本群可阅读") {
		t.Fatalf("dispatch: %+v %v", res, err)
	}
	if chats := ma.SharedChats(); len(chats) != 2 {
		t.Fatalf("queued grant missing: %v", chats)
	}
}
