package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in only. Creates one tiny private fixture and never posts a message.
// Existing documents and their sharing settings are not touched.
func TestLivePrivateAttachment(t *testing.T) {
	configPath := os.Getenv("DAIDAINIAO_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set DAIDAINIAO_LIVE_CONFIG to run a private test against Feishu")
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
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(filepath.Dir(configPath), dataDir)
	}
	cfg.DataDir = dataDir
	auth := NewAuth(cfg, false, filepath.Join(dataDir, "token.json"))
	auth.mu.Lock()
	valid := auth.tok != nil && time.Until(auth.tok.ExpiresAt) > 3*time.Minute
	auth.mu.Unlock()
	if !valid {
		t.Fatal("现有授权不足三分钟，请在应用内重新登录后运行验收；测试不主动刷新运行中应用的凭据")
	}
	client := NewFeishuClient(cfg.BaseURL, cfg.AppID, cfg.AppSecret, auth, cfg.ResourceAuth)
	svc := NewService(client, NewCache(filepath.Join(t.TempDir(), "cache")), cfg, auth)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	topic := fmt.Sprintf("私密权限验收_%d", time.Now().UnixNano())
	st, err := svc.Stage("私密权限验收.txt", bytes.NewBufferString("Daidainiao private attachment acceptance fixture\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Unstage(st.ID)
	defer func() {
		saved, _, err := svc.loadStaged(st.ID)
		if err != nil || saved.DriveToken == "" {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := client.callReq(cleanup, idUser, http.MethodDelete, "/drive/v1/files/"+url.PathEscape(saved.DriveToken), url.Values{"type": {"file"}}, nil, true, nil); err != nil {
			t.Errorf("测试文件清理失败：%v", err)
		}
		if folder, _, err := client.ensurePrivateTopicFolder(cleanup, topic); err == nil {
			if page, err := client.DriveList(cleanup, folder, ""); err == nil && len(page.Files) == 0 && !page.HasMore {
				if err := client.callReq(cleanup, idUser, http.MethodDelete, "/drive/v1/files/"+url.PathEscape(folder), url.Values{"type": {"folder"}}, nil, true, nil); err != nil {
					t.Errorf("测试目录清理失败：%v", err)
				}
			}
		}

	}()
	_, data, err := svc.loadStaged(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	bots := svc.resolveBotIDs(ctx, composeTarget{}, nil)
	part, err := svc.uploadOne(ctx, st, data, "", topic, bots, false)
	if err != nil {
		saved, _, _ := svc.loadStaged(st.ID)
		if saved != nil && saved.DriveToken != "" {
			if perms, e := client.DrivePermissions(ctx, saved.DriveToken, "file"); e == nil {
				for _, m := range perms.Members {
					t.Logf("Permission identity shape: type=%s member_type=%s sameOwner=%v sameBot=%v idPrefix=%s perm=%s", m.Type, m.MemberType, m.ID == auth.UserID(), m.ID == bots[0], truncate(m.ID, 5), m.Perm)
				}
			}
		}
		t.Fatal(err)
	}
	d, err := client.DriveDownload(ctx, part.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(d.Body, 1024))
	d.Body.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("所有者读取验证失败")
	}
	t.Log("真实飞书：私密父目录、关闭链接分享、唯一呆呆鸟协作者及所有者下载已验证")
	// The stored canonical application identity is read back from Feishu.
	saved, _, _ := svc.loadStaged(st.ID)
	if saved.DriveBotMember != nil && saved.DriveBotMember.MemberType == "appid" && saved.DriveBotMember.ID == cfg.AppID {
		resp, _, err := client.roundTrip(ctx, idTenant, http.MethodGet, "/drive/v1/files/"+url.PathEscape(part.Key)+"/download", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if err != nil || !bytes.Equal(b, data) {
			t.Fatal("呆呆鸟应用下载验证失败")
		}
		t.Log("真实飞书：呆呆鸟应用身份下载已验证")
	} else {
		resp, _, err := client.roundTrip(ctx, idTenant, http.MethodGet, "/drive/v1/files/"+url.PathEscape(part.Key)+"/download", nil)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if bytes.Equal(b, data) {
				t.Fatal("未授权应用读取到了私密文件")
			}
			t.Log("真实飞书：非协作者应用无法下载测试附件")
		}
		t.Log("待验收：尚未以目标呆呆鸟凭据及未授权群成员账号实测访问")
	}
}
