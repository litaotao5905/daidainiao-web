package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is loaded from config.json and overridden by env / flags.
type Config struct {
	AppID            string   `json:"app_id"`
	AppSecret        string   `json:"app_secret"`
	BaseURL          string   `json:"base_url"`
	Addr             string   `json:"addr"`
	DataDir          string   `json:"data_dir"`
	MaxTopicsPerChat int      `json:"max_topics_per_chat"`
	HistoryPages     int      `json:"history_pages"`
	RedirectURI      string   `json:"redirect_uri"`
	AccountsURL      string   `json:"accounts_url"`
	Scopes           []string `json:"scopes"`
	// ResourceAuth: which identity downloads message resources:
	// "auto" (default: app identity first, then user identity), "tenant" or "user".
	ResourceAuth string `json:"resource_auth"`
	// DisableReply turns the reply composer off (read-only mode): the
	// im:message / im:message.send_as_user scopes are then not requested.
	DisableReply bool `json:"disable_reply"`
	// All enabled legacy modes (auto/im/drive) normalize to private Drive uploads.
	// off disables attachments. IM uploads never bypass private sharing.
	UploadMode string `json:"upload_mode"`
	// DownloadDir is where every downloadable file of an opened conversation is
	// stored: <DownloadDir>/<group>/<topic>/<original filename>. Empty means
	// <Documents>/FeishuTopics (see resolveDownloadDir).
	DownloadDir string `json:"download_dir"`
	// HostDownloadDir is only used when running in Docker: the host path that
	// DownloadDir is mounted from, shown by "复制文件路径".
	HostDownloadDir string `json:"host_download_dir"`
	// DisableDownload turns the automatic local download off (resources are
	// then only proxied from the data/cache directory as before).
	DisableDownload bool `json:"disable_download"`
	// Legacy sharing options are read for compatibility and normalized to bot_only.
	// BotOpenIDs must contain at most one application bot open_id.
	DriveShareMode string   `json:"drive_share_mode"`
	BotOpenIDs     []string `json:"bot_open_ids,omitempty"`
}

// DefaultScopes are the user-identity scopes requested at login.
var DefaultScopes = []string{
	"offline_access",
	"im:chat:readonly",
	"im:message:readonly",
	"im:message.group_msg:get_as_user",
	"contact:contact.base:readonly",
	"contact:user.base:readonly",
	// replying to a topic as the user (im/v1/messages/:message_id/reply)
	"im:message",
	"im:message.send_as_user",
}

// ReplyScopes are the user-identity scopes needed to send a reply: the API
// requires im:message and im:message.send_as_user together.
var ReplyScopes = []string{"im:message", "im:message.send_as_user"}

// Upload scopes (user identity). im/v1/images and im/v1/files accept either
// im:resource or im:resource:upload; the Drive route needs drive:drive (which
// covers upload, metadata and adding collaborators) or the narrower trio.
var (
	UploadScopesIM    = []string{"im:resource"}
	UploadScopesDrive = []string{"drive:drive"}
	// DriveNarrowScopes can replace drive:drive when listed together in `scopes`.
	DriveNarrowScopes = []string{"drive:file:upload", "drive:drive.metadata:readonly", "docs:permission.member:create", "docs:permission.member:retrieve", "docs:permission.setting:read", "docs:permission.setting:write_only", "space:folder:create", "space:document:retrieve"}
)

const (
	UploadAuto  = "auto"
	UploadIM    = "im"
	UploadDrive = "drive"
	UploadOff   = "off"
)

const (
	DriveShareBotOnly = "bot_only"
	DriveShareChat    = "chat"
	DriveShareAll     = "all"
	DriveShareNone    = "none"
)

// ScopeSendAsUser is the scope that decides whether replying is possible.
const ScopeSendAsUser = "im:message.send_as_user"

func defaultConfig() Config {
	return Config{
		BaseURL:          "https://open.feishu.cn",
		Addr:             "127.0.0.1:8080",
		DataDir:          "data",
		DriveShareMode:   DriveShareBotOnly,
		MaxTopicsPerChat: 30,
		HistoryPages:     6,
	}
}

// LoadConfig reads path if it exists (a missing file is fine).
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// ApplyEnv overrides config values from environment variables.
func (c *Config) ApplyEnv() {
	if v := os.Getenv("FEISHU_APP_ID"); v != "" {
		c.AppID = v
	}
	if v := os.Getenv("FEISHU_APP_SECRET"); v != "" {
		c.AppSecret = v
	}
	if v := os.Getenv("FEISHU_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("FEISHU_REDIRECT_URI"); v != "" {
		c.RedirectURI = v
	}
	if v := strings.ToLower(os.Getenv("FEISHU_DISABLE_REPLY")); v == "1" || v == "true" || v == "yes" {
		c.DisableReply = true
	}
	if v := strings.ToLower(os.Getenv("FEISHU_UPLOAD_MODE")); v != "" {
		c.UploadMode = v
	}
	if v := os.Getenv("FEISHU_DOWNLOAD_DIR"); v != "" {
		c.DownloadDir = v
	}
	if v := os.Getenv("FEISHU_HOST_DOWNLOAD_DIR"); v != "" {
		c.HostDownloadDir = v
	}
	if v := strings.ToLower(os.Getenv("FEISHU_DISABLE_DOWNLOAD")); v == "1" || v == "true" || v == "yes" {
		c.DisableDownload = true
	}
	if v := os.Getenv("ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("FEISHU_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := strings.ToLower(os.Getenv("FEISHU_DRIVE_SHARE_MODE")); v != "" {
		c.DriveShareMode = v
	}
}

// Normalize fills defaults for zero values.
func (c *Config) Normalize() {
	d := defaultConfig()
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.BaseURL == "" {
		c.BaseURL = d.BaseURL
	}
	if c.Addr == "" {
		c.Addr = d.Addr
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.MaxTopicsPerChat <= 0 {
		c.MaxTopicsPerChat = d.MaxTopicsPerChat
	}
	if c.HistoryPages <= 0 {
		c.HistoryPages = d.HistoryPages
	}
	c.DriveShareMode = DriveShareBotOnly
	switch strings.ToLower(strings.TrimSpace(c.UploadMode)) {
	case UploadOff, "none", "disabled":
		c.UploadMode = UploadOff
	default:
		c.UploadMode = UploadDrive
	}
	if c.DisableReply {
		c.UploadMode = UploadOff
	}
	if len(c.Scopes) == 0 {
		c.Scopes = append([]string{}, DefaultScopes...)
		if c.DisableReply {
			c.Scopes = removeScopes(c.Scopes, ReplyScopes)
		}
	}
	if !c.DisableReply {
		// a config.json copied from an older example lists scopes explicitly;
		// make sure replying / uploading keeps working without hand-editing it
		for _, sc := range ReplyScopes {
			if !containsScope(c.Scopes, sc) {
				c.Scopes = append(c.Scopes, sc)
			}
		}
		if c.UploadMode == UploadDrive && !containsScope(c.Scopes, "drive:drive") {
			for _, sc := range DriveNarrowScopes {
				if !containsScope(c.Scopes, sc) {
					c.Scopes = append(c.Scopes, sc)
				}
			}
		}
	}
	if !c.DisableDownload {
		for _, sc := range []string{"drive:file:download", "drive:drive.metadata:readonly", "space:document:retrieve", "wiki:node:read", "docs:document:export"} {
			if !containsScope(c.Scopes, sc) {
				c.Scopes = append(c.Scopes, sc)
			}
		}
	}
	if c.RedirectURI == "" {
		port := "8080"
		if _, p, err := net.SplitHostPort(c.Addr); err == nil && p != "" {
			port = p
		}
		c.RedirectURI = "http://localhost:" + port + "/callback"
	}
	if c.ResourceAuth == "" {
		c.ResourceAuth = "auto"
	}
}

func containsScope(list []string, sc string) bool {
	for _, x := range list {
		if x == sc {
			return true
		}
	}
	return false
}

func containsAll(list, need []string) bool {
	for _, x := range need {
		if !containsScope(list, x) {
			return false
		}
	}
	return true
}

func removeScopes(list, drop []string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if !containsScope(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// ReplyEnabled reports whether the reply composer is available.
func (c *Config) ReplyEnabled() bool { return !c.DisableReply }

// UploadEnabled reports whether attachments can be added in the composer.
func (c *Config) UploadEnabled() bool { return !c.DisableReply && c.UploadMode != UploadOff }

// uploadModeOrAuto tolerates configs that were not normalized (tests).
func (c *Config) uploadModeOrAuto() string {
	if c.UploadMode == UploadOff {
		return UploadOff
	}
	return UploadDrive
}
func (c *Config) driveShareModeOrBotOnly() string { return DriveShareBotOnly }

func (c *Config) Configured() bool { return c.AppID != "" && c.AppSecret != "" }

// Selection persists which group chats the user picked.
type Selection struct {
	mu   sync.Mutex
	path string
	ids  []string
}

type selectionFile struct {
	ChatIDs []string `json:"chat_ids"`
}

// LoadSelection loads path; exists reports whether a file was found.
func LoadSelection(path string) (*Selection, bool, error) {
	s := &Selection{path: path, ids: []string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, false, nil
		}
		return s, false, err
	}
	var f selectionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return s, true, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if f.ChatIDs != nil {
		s.ids = f.ChatIDs
	}
	return s, true, nil
}

func (s *Selection) Get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.ids...)
}

func (s *Selection) Set(ids []string) error {
	seen := map[string]bool{}
	clean := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.MarshalIndent(selectionFile{ChatIDs: clean}, "", "  ")
	if err := writeFileAtomic(s.path, b, 0o600); err != nil {
		return err
	}
	s.ids = clean
	return nil
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// uploadScopeOK reports whether a token's granted scopes allow the configured
// upload route(s). An empty list (unknown) is assumed to be fine.
func (c *Config) uploadScopeOK(granted []string) bool {
	if len(granted) == 0 {
		return true
	}
	return c.uploadModeOrAuto() != UploadOff && (containsScope(granted, "drive:drive") || containsAll(granted, DriveNarrowScopes))
}
