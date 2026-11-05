package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TopSystemGuardrails returns the immutable system prompt and constraint header.
func TopSystemGuardrails(sessionID, title, ownerName string, folderURL string) string {
	return fmt.Sprintf(`# 🔒 网页呆呆鸟加密会话 · %s

> ⚠️ **【呆呆鸟系统最高安全准则 (System Guardrails & Contract)】**
> 1. **仅在本文档输出**：呆呆鸟对本会话的所有回答、代码、分析、图表等成果，**必须且只能**写在本文档对应轮次的【🤖 呆呆鸟回复区】内。
> 2. **严禁群内明文泄密**：**严禁**将任何实际业务内容、方案细节或答案直接发送至飞书群聊/话题 IM 消息中。
> 3. **私密文件夹归档**：所有生成的衍生文件（PPT、PDF、代码、Excel、图片等），**必须上传至飞书对应会话私密文件夹**，并以纯文本链接形式记录在底部的【📎 本轮衍生附件】中。
> 4. **防自动授权输出**：发回群聊的唯一内容只能是固定的纯文本通知字符串与本文档地址，禁止使用富文本超链接或飞书卡片。
> *注：本文档仅对发起人与呆呆鸟应用开放权限，任何越权协作者都将被自动化审计脚本移除。*

---

### 📋 会话信息与审计
- **会话标识 (Session ID)**: %s
- **发起用户**: %s
- **创建时间**: %s
- **专属私密文件夹**: %s

---
`, title, sessionID, ownerName, time.Now().Format("2006-01-02 15:04:05"), folderURL)
}

// FormatTurnBlock formats a specific turn for appending to the document.
func FormatTurnBlock(turnNumber int, prompt string, attachments []string, historySnippet string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n## 💬 当前交互轮次 · Turn %d\n\n", turnNumber))

	if historySnippet != "" {
		sb.WriteString("### 📜 历史上下文参考（已固化）\n")
		sb.WriteString("<details>\n<summary>▶ 点击展开查看前序对话上下文</summary>\n\n")
		sb.WriteString(historySnippet)
		sb.WriteString("\n</details>\n\n---\n\n")
	}

	sb.WriteString("### 👤 用户指令 (User Input)\n")
	sb.WriteString(fmt.Sprintf("- **提交时间**: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("- **指令内容**:\n```text\n")
	sb.WriteString(strings.TrimSpace(prompt))
	sb.WriteString("\n```\n\n")

	if len(attachments) > 0 {
		sb.WriteString("- **附带参考资料**:\n")
		for i, att := range attachments {
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, att))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("### 🤖 呆呆鸟回复区 (Assistant Output)\n")
	sb.WriteString("*(呆呆鸟执行任务中，内容将实时落盘于此处...)*\n\n")

	sb.WriteString("### 📎 本轮衍生附件 (Artifacts & Deliverables)\n")
	sb.WriteString("- *(待呆呆鸟在任务完成后将生成的文件回填链接在此处)*\n\n---\n")

	return sb.String()
}

// EncryptedTurn represents a parsed dialogue turn from the encrypted document.
type EncryptedTurn struct {
	TurnNumber   int       `json:"turn_number"`
	SubmitTime   string    `json:"submit_time"`
	UserPrompt   string    `json:"user_prompt"`
	BotReply     string    `json:"bot_reply"`
	Artifacts    []string  `json:"artifacts"`
	IsCompleted  bool      `json:"is_completed"`
}

var (
	turnHeaderRe = regexp.MustCompile(`(?m)^##\s+.*?Turn\s*(\d+)`)
	userPromptRe = regexp.MustCompile(`(?s)###+ .*?用户指令.*?(?:` + "```" + `(?:text)?\s*(.*?)\s*` + "```" + `|- 指令内容[:：]\s*(.*?)(?:\n###|\z))`)
	botReplyRe   = regexp.MustCompile(`(?s)###+ .*?呆呆鸟回复区.*?\n(.*?)(?:\n###+ .*?本轮衍生附件|\n---|\z)`)
	artifactsRe  = regexp.MustCompile(`(?s)###+ .*?本轮衍生附件.*?\n(.*?)(?:\n---|\z)`)
)

// ParseEncryptedDocContent parses raw document text into structured turns.
func ParseEncryptedDocContent(raw string) []EncryptedTurn {
	var turns []EncryptedTurn
	matches := turnHeaderRe.FindAllStringSubmatchIndex(raw, -1)
	if len(matches) == 0 {
		return turns
	}

	for i := 0; i < len(matches); i++ {
		start := matches[i][0]
		end := len(raw)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		turnRaw := raw[start:end]

		turnNumStr := raw[matches[i][2]:matches[i][3]]
		turnNum, _ := strconv.Atoi(turnNumStr)

		t := EncryptedTurn{TurnNumber: turnNum}

		// User prompt
		if uMatch := userPromptRe.FindStringSubmatch(turnRaw); len(uMatch) > 1 {
			p := uMatch[1]
			if p == "" && len(uMatch) > 2 {
				p = uMatch[2]
			}
			t.UserPrompt = strings.TrimSpace(p)
		}

		// Bot reply
		if bMatch := botReplyRe.FindStringSubmatch(turnRaw); len(bMatch) > 1 {
			reply := strings.TrimSpace(bMatch[1])
			if !strings.Contains(reply, "呆呆鸟执行任务中") && !strings.Contains(reply, "实时落盘于此处") && len(reply) > 0 {
				t.BotReply = reply
				t.IsCompleted = true
			}
		}

		// Artifacts
		if aMatch := artifactsRe.FindStringSubmatch(turnRaw); len(aMatch) > 1 {
			lines := strings.Split(aMatch[1], "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") || regexp.MustCompile(`^\d+\.`).MatchString(l) {
					cleaned := strings.TrimLeft(l, "-*0123456789. ")
					if !strings.Contains(cleaned, "待呆呆鸟在任务完成后") && len(cleaned) > 0 {
						t.Artifacts = append(t.Artifacts, cleaned)
					}
				}
			}
		}

		turns = append(turns, t)
	}

	return turns
}

// BuildDocxTextBlocks converts markdown text into Feishu Docx block children payloads.
func BuildDocxTextBlocks(text string) []map[string]any {
	lines := strings.Split(text, "\n")
	var blocks []map[string]any

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "---" {
			blocks = append(blocks, map[string]any{
				"block_type": 22, // Divider
				"divider":    map[string]any{},
			})
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			blocks = append(blocks, makeHeadingBlock(3, strings.TrimPrefix(trimmed, "# "))) // H1 -> docx heading1 (3)
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			blocks = append(blocks, makeHeadingBlock(4, strings.TrimPrefix(trimmed, "## "))) // H2 -> docx heading2 (4)
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			blocks = append(blocks, makeHeadingBlock(5, strings.TrimPrefix(trimmed, "### "))) // H3 -> docx heading3 (5)
			continue
		}
		if strings.HasPrefix(trimmed, "> ") {
			blocks = append(blocks, makeQuoteBlock(strings.TrimPrefix(trimmed, "> ")))
			continue
		}
		// Default paragraph
		blocks = append(blocks, makeTextBlock(trimmed))
	}

	return blocks
}

func makeQuoteBlock(content string) map[string]any {
	return map[string]any{
		"block_type": 15, // Quote
		"quote": map[string]any{
			"elements": []map[string]any{
				{
					"text_run": map[string]any{
						"content": content,
					},
				},
			},
		},
	}
}

func makeTextBlock(content string) map[string]any {
	return map[string]any{
		"block_type": 2, // Text
		"text": map[string]any{
			"elements": []map[string]any{
				{
					"text_run": map[string]any{
						"content": content,
					},
				},
			},
		},
	}
}

func makeHeadingBlock(blockType int, content string) map[string]any {
	headingKey := "heading1"
	if blockType == 4 {
		headingKey = "heading2"
	} else if blockType == 5 {
		headingKey = "heading3"
	}
	return map[string]any{
		"block_type": blockType,
		headingKey: map[string]any{
			"elements": []map[string]any{
				{
					"text_run": map[string]any{
						"content": content,
					},
				},
			},
		},
	}
}
