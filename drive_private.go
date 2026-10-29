package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DrivePermissionSnapshot is read back from Feishu, never inferred from a write.
type DrivePermissionSnapshot struct {
	LinkShare    string                  `json:"link_share_entity"`
	ShareEntity  string                  `json:"share_entity"`
	ManageEntity string                  `json:"manage_collaborator_entity"`
	Members      []DrivePermissionMember `json:"members"`
}
type DrivePermissionMember struct {
	ID         string `json:"member_id"`
	MemberType string `json:"member_type"`
	Perm       string `json:"perm"`
	Type       string `json:"type"`
}

func (c *FeishuClient) DriveClosePrivate(ctx context.Context, token, kind string) error {
	body, _ := json.Marshal(map[string]any{
		"link_share_entity": "closed", "share_entity": "same_tenant",
		"manage_collaborator_entity": "collaborator_full_access",
	})
	return c.callReq(ctx, idUser, http.MethodPatch, "/drive/v2/permissions/"+url.PathEscape(token)+"/public", url.Values{"type": {kind}}, body, true, nil)
}

func (c *FeishuClient) DrivePermissions(ctx context.Context, token, kind string) (*DrivePermissionSnapshot, error) {
	q := url.Values{"type": {kind}}
	var pub struct {
		Public *DrivePermissionSnapshot `json:"permission_public"`
	}
	if err := c.call(ctx, idUser, "/drive/v2/permissions/"+url.PathEscape(token)+"/public", q, &pub); err != nil {
		return nil, err
	}
	if pub.Public == nil {
		return nil, fmt.Errorf("飞书没有返回权限设置，无法确认文件私密性")
	}
	q.Set("fields", "type")
	var members struct {
		Items []DrivePermissionMember `json:"items"`
	}
	if err := c.call(ctx, idUser, "/drive/v1/permissions/"+url.PathEscape(token)+"/members", q, &members); err != nil {
		return nil, err
	}
	pub.Public.Members = members.Items
	return pub.Public, nil
}

func isChatView(m DrivePermissionMember, chatID string) bool {
	if chatID == "" || m.ID != chatID || m.Perm != "view" || m.MemberType != "openchat" {
		return false
	}
	return m.Type == "" || m.Type == "chat"
}

// chatID empty means the file must stay private. A non-empty chatID requires
// exactly that chat to have view, and still rejects every other collaborator.
func verifyPrivatePermissions(p *DrivePermissionSnapshot, owner, bot, chatID string) error {
	if owner == "" || p == nil || p.LinkShare != "closed" || p.ShareEntity != "same_tenant" || p.ManageEntity != "collaborator_full_access" {
		return fmt.Errorf("未确认链接分享已关闭且仅所有者可管理协作者")
	}
	if bot != "" && !strings.Contains(bot, ":") {
		bot = "openid:" + bot
	}
	botFound := bot == ""
	chatFound := chatID == ""
	for _, m := range p.Members {
		if m.MemberType == "openid" && m.ID == owner {
			continue
		}
		if bot != "" && m.MemberType+":"+m.ID == bot && m.Perm == "view" && m.Type == "app" {
			botFound = true
			continue
		}
		if isChatView(m, chatID) {
			if chatFound {
				return fmt.Errorf("检测到重复的群阅读权限，停止发送")
			}
			chatFound = true
			continue
		}
		return fmt.Errorf("检测到非目标协作者、群组或超出阅读范围的权限，停止发送")
	}
	if !botFound {
		return fmt.Errorf("未确认呆呆鸟应用的阅读权限")
	}
	if !chatFound {
		return fmt.Errorf("未确认当前群的阅读权限，停止发送")
	}
	return nil
}

// Private folders never fall back to a potentially shared legacy folder.
func (c *FeishuClient) privateFolder(ctx context.Context, parent, name string) (string, error) {
	page := ""
	seen := map[string]bool{}
	for {
		if seen[page] {
			return "", fmt.Errorf("附件目录分页重复")
		}
		seen[page] = true
		p, err := c.DriveList(ctx, parent, page)
		if err != nil {
			return "", err
		}
		for _, f := range p.Files {
			if f.Type != "folder" || f.Name != name {
				continue
			}
			if f.OwnerID != c.auth.UserID() {
				return "", fmt.Errorf("附件目录不是当前登录用户所有，停止上传")
			}
			p, err := c.DrivePermissions(ctx, f.Token, "folder")
			if err != nil {
				return "", err
			}
			if err = verifyPrivateFolder(p, c.auth.UserID()); err != nil {
				return "", fmt.Errorf("附件父目录权限不符合要求：%w", err)
			}
			return f.Token, nil
		}
		if !p.HasMore {
			break
		}
		if p.NextPage == "" || p.NextPage == page {
			return "", fmt.Errorf("附件目录分页不完整")
		}
		page = p.NextPage
	}
	body, _ := json.Marshal(map[string]string{"name": name, "folder_token": parent})
	var f struct {
		Token string `json:"token"`
	}
	if err := c.callReq(ctx, idUser, http.MethodPost, "/drive/v1/files/create_folder", nil, body, true, &f); err != nil {
		return "", err
	}
	if f.Token == "" {
		return "", fmt.Errorf("创建私密目录未返回 token")
	}
	p, err := c.DrivePermissions(ctx, f.Token, "folder")
	if err != nil {
		return "", err
	}
	if err := verifyPrivateFolder(p, c.auth.UserID()); err != nil {
		return "", err
	}
	return f.Token, nil
}

func (c *FeishuClient) ensurePrivateTopicFolder(ctx context.Context, topic string) (string, string, error) {
	c.folderMu.Lock()
	defer c.folderMu.Unlock()
	root, err := c.rootFolder(ctx)
	if err != nil {
		return "", "", err
	}
	base, err := c.privateFolder(ctx, root, DrivePrivateRootName)
	if err != nil {
		return "", "", err
	}
	if topic == "" {
		return base, DrivePrivateRootName, nil
	}
	folder, err := c.privateFolder(ctx, base, topic)
	return folder, DrivePrivateRootName + "/" + topic, err
}

func (c *FeishuClient) VerifyUploadParents(ctx context.Context, topic string) error {
	_, _, err := c.ensurePrivateTopicFolder(ctx, topic)
	return err
}

// Folder settings are read through v2; public-setting PATCH does not document
// folder support. A newly created folder must already be private or we stop.
func verifyPrivateFolder(p *DrivePermissionSnapshot, owner string) error {
	if p == nil || p.LinkShare != "closed" || owner == "" {
		return fmt.Errorf("无法确认父文件夹为私密目录，停止上传")
	}
	for _, m := range p.Members {
		if m.MemberType != "openid" || m.ID != owner {
			return fmt.Errorf("父文件夹存在其他协作者，停止上传")
		}
	}
	return nil
}

// A successful open_id grant can be echoed as open_id by create, while list
// normalizes it to appid. Only a unique application added to an owner-only
// file is accepted; any other collaborator makes the verification fail.
func canonicalBotMember(p *DrivePermissionSnapshot, owner, botOpenID string, granted *DrivePermissionMember, allowChat string) (*DrivePermissionMember, error) {
	if p == nil || granted == nil {
		return nil, fmt.Errorf("未确认呆呆鸟授权结果")
	}
	if granted.MemberType != "appid" && !(granted.MemberType == "openid" && granted.ID == botOpenID) {
		return nil, fmt.Errorf("授权目标身份不一致")
	}
	var target *DrivePermissionMember
	for _, m := range p.Members {
		if m.MemberType == "openid" && m.ID == owner {
			continue
		}
		if isChatView(m, allowChat) {
			continue
		}
		if m.Type != "app" || m.Perm != "view" || (m.MemberType != "appid" && !(m.MemberType == "openid" && m.ID == botOpenID)) || target != nil {
			return nil, fmt.Errorf("附件存在非目标应用、群组或额外协作者，停止发送")
		}
		if granted.MemberType == "appid" && (m.MemberType != granted.MemberType || m.ID != granted.ID) {
			return nil, fmt.Errorf("授权应用与权限读取结果不一致")
		}
		copy := m
		target = &copy
	}
	if target == nil {
		return nil, fmt.Errorf("未读取到呆呆鸟应用的阅读权限")
	}
	return target, nil
}
