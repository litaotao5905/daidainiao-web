package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NotificationSettings stores user preferences for notifications.
type NotificationSettings struct {
	FeishuEnabled  bool `json:"feishu_enabled"`
	BrowserEnabled bool `json:"browser_enabled"`
	SoundEnabled   bool `json:"sound_enabled"`
}

// WatchedTask tracks an active Daidainiao task in a thread.
type WatchedTask struct {
	ThreadID   string `json:"thread_id"`
	ChatID     string `json:"chat_id"`
	ChatName   string `json:"chat_name"`
	TopicTitle string `json:"topic_title"`
	UserOpenID string `json:"user_open_id"`
	StartedAt  int64  `json:"started_at"`
	WasBusy    bool   `json:"was_busy"`
	Notified   bool   `json:"notified"`
}

// NotifyManager coordinates task completion notifications across Feishu, browser, and sound.
type NotifyManager struct {
	mu           sync.Mutex
	svc          *Service
	settings     NotificationSettings
	settingsPath string
	watched      map[string]*WatchedTask
	lastNotified map[string]int64
	workerRun    bool
	wakeCh       chan struct{}
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

func newNotifyManager(svc *Service) *NotifyManager {
	dir := ""
	if svc != nil {
		dir = svc.cfg.DataDir
	}
	p := ""
	if dir != "" {
		p = filepath.Join(dir, "settings", "notifications.json")
	}
	m := &NotifyManager{
		svc:          svc,
		settingsPath: p,
		watched:      make(map[string]*WatchedTask),
		lastNotified: make(map[string]int64),
		wakeCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		settings: NotificationSettings{
			FeishuEnabled:  true,
			BrowserEnabled: true,
			SoundEnabled:   true,
		},
	}
	m.loadSettings()
	return m
}

func (m *NotifyManager) loadSettings() {
	if m.settingsPath == "" {
		return
	}
	b, err := os.ReadFile(m.settingsPath)
	if err != nil {
		return
	}
	var s NotificationSettings
	if json.Unmarshal(b, &s) == nil {
		m.settings = s
	}
}

func (m *NotifyManager) saveSettingsLocked() {
	if m.settingsPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.settingsPath), 0o700)
	b, err := json.MarshalIndent(m.settings, "", "  ")
	if err == nil {
		_ = writeFileAtomic(m.settingsPath, b, 0o600)
	}
}

// Settings returns the current notification settings.
func (m *NotifyManager) Settings() NotificationSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// UpdateSettings updates and persists the notification settings.
func (m *NotifyManager) UpdateSettings(s NotificationSettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings = s
	m.saveSettingsLocked()
}

// Watch registers interest in a Daidainiao task running in threadID.
func (m *NotifyManager) Watch(threadID, chatID, chatName, topicTitle, userOpenID string) {
	if threadID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if userOpenID == "" && m.svc != nil && m.svc.auth != nil {
		userOpenID = m.svc.auth.UserID()
	}
	task, exists := m.watched[threadID]
	if !exists {
		task = &WatchedTask{
			ThreadID:  threadID,
			StartedAt: time.Now().UnixMilli(),
			WasBusy:   true,
		}
		m.watched[threadID] = task
	}
	if chatID != "" {
		task.ChatID = chatID
	}
	if chatName != "" {
		task.ChatName = chatName
	}
	if topicTitle != "" {
		task.TopicTitle = topicTitle
	}
	if userOpenID != "" {
		task.UserOpenID = userOpenID
	}
	m.ensureWorkerLocked()
}

// Unwatch stops watching threadID.
func (m *NotifyManager) Unwatch(threadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watched, threadID)
}

// Stop terminates any running background worker and clears watched tasks.
func (m *NotifyManager) Stop() {
	m.mu.Lock()
	clear(m.watched)
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *NotifyManager) ensureWorkerLocked() {
	if m.workerRun || len(m.watched) == 0 {
		return
	}
	m.workerRun = true
	m.wg.Add(1)
	go m.runWorker()
}

func (m *NotifyManager) runWorker() {
	defer func() {
		m.mu.Lock()
		m.workerRun = false
		m.mu.Unlock()
		m.wg.Done()
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		count := len(m.watched)
		m.mu.Unlock()
		if count == 0 {
			return
		}

		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkWatchedTasks()
		case <-m.wakeCh:
			m.checkWatchedTasks()
		}
	}
}

func (m *NotifyManager) checkWatchedTasks() {
	m.mu.Lock()
	tasks := make([]*WatchedTask, 0, len(m.watched))
	for _, t := range m.watched {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()

	now := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	for _, t := range tasks {
		// Clean up tasks older than 3 hours
		if now-t.StartedAt > 3*3600*1000 {
			m.Unwatch(t.ThreadID)
			continue
		}

		raws, err := m.svc.threadMessages(ctx, t.ThreadID, m.svc.knownRoot(t.ThreadID), true)
		if err != nil || len(raws) == 0 {
			continue
		}
		r := m.svc.render(ctx, raws)
		busy, _ := m.svc.topicBusyState(t.ChatName, r.msgs, m.svc.getLastSent(t.ThreadID), time.Now())

		if busy {
			t.WasBusy = true
			continue
		}

		// Transitioned from busy -> finished!
		if t.WasBusy && !t.Notified {
			t.Notified = true
			snippet := extractLatestSnippet(r.msgs)
			if t.TopicTitle == "" && len(r.msgs) > 0 {
				for _, m := range r.msgs {
					if m.IsRoot {
						t.TopicTitle = firstLine(m.Plain, 60)
						break
					}
				}
				if t.TopicTitle == "" {
					t.TopicTitle = firstLine(r.msgs[0].Plain, 60)
				}
			}
			webBase := m.svc.resolveWebBaseURL()
			_, _ = m.NotifyTaskComplete(ctx, t.ThreadID, t.ChatName, t.TopicTitle, t.UserOpenID, snippet, webBase)
			m.Unwatch(t.ThreadID)
		}
	}
}

func extractLatestSnippet(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.SenderType == "app" && !m.Deleted {
			p := strings.TrimSpace(m.Plain)
			if p != "" {
				return truncateRunes(p, 180)
			}
		}
	}
	return "本轮任务已执行结束，可前往话题查看最新生成结果与文件。"
}

func (s *Service) resolveWebBaseURL() string {
	if s.cfg.RedirectURI != "" {
		if u, err := url.Parse(s.cfg.RedirectURI); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	if s.cfg.Addr != "" {
		addr := s.cfg.Addr
		if strings.HasPrefix(addr, "0.0.0.0:") {
			addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
		}
		if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
			addr = "http://" + addr
		}
		return addr
	}
	return "http://localhost:8088"
}

// NotifyTaskComplete sends a Feishu card notification if enabled, de-duplicating multiple calls.
func (m *NotifyManager) NotifyTaskComplete(ctx context.Context, threadID, chatName, topicTitle, userOpenID, snippet, webBaseURL string) (bool, error) {
	if threadID == "" {
		return false, errors.New("threadID 不能为空")
	}

	m.mu.Lock()
	now := time.Now().UnixMilli()
	if last := m.lastNotified[threadID]; last > 0 && now-last < 45_000 {
		m.mu.Unlock()
		return false, nil // deduplicated
	}
	m.lastNotified[threadID] = now
	enabled := m.settings.FeishuEnabled
	m.mu.Unlock()

	if !enabled {
		return true, nil
	}

	if userOpenID == "" && m.svc != nil && m.svc.auth != nil {
		userOpenID = m.svc.auth.UserID()
	}
	if userOpenID == "" {
		return false, errors.New("用户未登录或缺少 open_id，无法发送飞书私聊通知")
	}

	if webBaseURL == "" && m.svc != nil {
		webBaseURL = m.svc.resolveWebBaseURL()
	}
	card := buildTaskCompleteCard(chatName, topicTitle, threadID, snippet, webBaseURL)
	if m.svc != nil && m.svc.api != nil {
		if err := m.svc.api.SendBotCard(ctx, userOpenID, card); err != nil {
			log.Printf("[notify] 发送飞书私聊通知失败 (thread=%s user=%s): %v", threadID, userOpenID, err)
			return false, err
		}
		log.Printf("[notify] 已成功发送飞书任务完成通知 -> user=%s thread=%s", userOpenID, threadID)
	}
	return true, nil
}

// SendTestNotice sends an immediate test notification card to userOpenID.
func (m *NotifyManager) SendTestNotice(ctx context.Context, userOpenID, webBaseURL string) error {
	if userOpenID == "" && m.svc != nil && m.svc.auth != nil {
		userOpenID = m.svc.auth.UserID()
	}
	if userOpenID == "" {
		return errors.New("未检测到登录用户，请先登录飞书账号")
	}
	if webBaseURL == "" && m.svc != nil {
		webBaseURL = m.svc.resolveWebBaseURL()
	}
	card := buildTestCard(webBaseURL)
	if m.svc != nil && m.svc.api != nil {
		return m.svc.api.SendBotCard(ctx, userOpenID, card)
	}
	return nil
}

func buildTaskCompleteCard(chatName, topicTitle, threadID, snippet, webURL string) map[string]any {
	if topicTitle == "" {
		topicTitle = "未命名话题"
	}
	if chatName == "" {
		chatName = "呆呆鸟话题群"
	}
	if snippet == "" {
		snippet = "本轮任务已执行结束，可前往话题查看最新生成结果与文件。"
	}
	fullURL := strings.TrimRight(webURL, "/") + "/#t=" + url.QueryEscape(threadID)
	nowStr := time.Now().Format("15:04:05")

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "turquoise",
			"title": map[string]any{
				"tag":     "plain_text",
				"content": "🔔 呆呆鸟任务已完成",
			},
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**话题**\n" + topicTitle,
						},
					},
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**所属群组**\n" + chatName,
						},
					},
				},
			},
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**完成时间**\n" + nowStr,
						},
					},
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**状态**\n✅ 本轮执行结束",
						},
					},
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": "**最新进展摘要**\n" + snippet,
				},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": "👉 在网页版中打开话题"},
						"type": "primary",
						"multi_url": map[string]any{
							"url":         fullURL,
							"pc_url":      fullURL,
							"android_url": fullURL,
							"ios_url":     fullURL,
						},
					},
				},
			},
		},
	}
}

func buildTestCard(webURL string) map[string]any {
	fullURL := strings.TrimRight(webURL, "/") + "/"
	nowStr := time.Now().Format("2006-01-02 15:04:05")

	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "green",
			"title": map[string]any{
				"tag":     "plain_text",
				"content": "🔔 呆呆鸟任务通知测试成功",
			},
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": "恭喜！**飞书自建应用机器人私聊通知功能已成功连通** 👏\n\n后续当呆呆鸟在任何话题群中完成思考、编写代码或生成报告后，系统都会直接通过此单聊卡片向你发送强提醒（电脑和手机飞书均会响铃、弹窗、亮红点）。",
				},
			},
			map[string]any{
				"tag": "div",
				"fields": []any{
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**测试时间**\n" + nowStr,
						},
					},
					map[string]any{
						"is_short": true,
						"text": map[string]any{
							"tag":     "lark_md",
							"content": "**通知渠道**\n飞书机器人 1对1 私聊",
						},
					},
				},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": "进入网页呆呆鸟"},
						"type": "primary",
						"multi_url": map[string]any{
							"url":         fullURL,
							"pc_url":      fullURL,
							"android_url": fullURL,
							"ios_url":     fullURL,
						},
					},
				},
			},
		},
	}
}
