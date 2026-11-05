package main

import (
	"context"
	"fmt"
	"strings"
)

// VerifyStrictEncryptedPermissions asserts that a Feishu Docx document is strictly private.
func (c *FeishuClient) VerifyStrictEncryptedPermissions(ctx context.Context, docToken, ownerID, botID string) error {
	snap, err := c.DrivePermissions(ctx, docToken, "docx")
	if err != nil {
		return fmt.Errorf("读取文档权限状态失败：%w", err)
	}
	return VerifyStrictPermissionsSnapshot(snap, ownerID, botID)
}

// VerifyStrictPermissionsSnapshot audits a permission snapshot:
// - Link sharing must be "closed"
// - Members can ONLY contain the owner (user open_id) and the Daidainiao bot
// - Any chat (openchat) or unauthorized third-party user causes an immediate error
func VerifyStrictPermissionsSnapshot(snap *DrivePermissionSnapshot, ownerID, botID string) error {
	if ownerID == "" {
		return fmt.Errorf("无法确认当前文档所有者身份")
	}
	if botID == "" {
		botID = RedDaidainiaoBotOpenID
	}
	if !strings.Contains(botID, ":") {
		botID = "openid:" + botID
	}
	if snap == nil {
		return fmt.Errorf("飞书未返回权限状态")
	}

	// 1. Link sharing must be strictly closed
	if snap.LinkShare != "closed" {
		return fmt.Errorf("安全违规：文档链接分享未关闭 (当前为 %s)，存在泄露风险", snap.LinkShare)
	}

	// 2. Member list audit
	ownerFound := false
	botFound := false

	for _, m := range snap.Members {
		// Is owner?
		if m.MemberType == "openid" && m.ID == ownerID {
			ownerFound = true
			continue
		}
		// Is bot? (Feishu lists application collaborators as member_type=="appid" or type=="app")
		if ((m.MemberType == "appid" || m.Type == "app") || (m.MemberType+":"+m.ID == botID || m.ID == strings.TrimPrefix(botID, "openid:"))) && (m.Perm == "edit" || m.Perm == "view" || m.Perm == "full_access") {
			botFound = true
			continue
		}
		// Any chat is strictly prohibited!
		if m.MemberType == "openchat" {
			return fmt.Errorf("严重安全违规：检测到文档被授予了群组权限 (%s)，拒绝交互", m.ID)
		}
		// Any unauthorized user
		return fmt.Errorf("安全审计拦截：文档存在非白名单协作者 (%s: %s)，已阻止交互", m.MemberType, m.ID)
	}

	if !ownerFound {
		return fmt.Errorf("未确认文档所有者权限")
	}
	if !botFound {
		return fmt.Errorf("未确认呆呆鸟机器人的编辑/阅读权限")
	}

	return nil
}

// EnsureDocxStrictlyPrivate sets LinkShare to closed and adds the bot as an editor.
func (c *FeishuClient) EnsureDocxStrictlyPrivate(ctx context.Context, docToken, botID string) error {
	if err := c.DriveClosePrivate(ctx, docToken, "docx"); err != nil {
		return fmt.Errorf("关闭文档链接分享失败：%w", err)
	}
	if botID == "" {
		botID = RedDaidainiaoBotOpenID
	}
	// Grant edit permission to bot so it can write assistant response into the docx
	if _, err := c.DriveShareMemberType(ctx, docToken, "docx", "openid", botID, "edit"); err != nil {
		// Some tenants or configurations might return duplicate or app error; ignore if already a member
		if !strings.Contains(err.Error(), "already") && !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "1061001") {
			return fmt.Errorf("授予呆呆鸟机器人文档协作者权限失败：%w", err)
		}
	}
	return nil
}
