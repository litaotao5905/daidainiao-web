package main

// Chat is a group chat the bot is a member of.
type Chat struct {
	ChatID      string `json:"chat_id"`
	Name        string `json:"name"`
	Avatar      string `json:"avatar,omitempty"`
	Description string `json:"description,omitempty"`
	External    bool   `json:"external,omitempty"`
	ChatStatus  string `json:"chat_status,omitempty"`
}

// User is the subset of contact info we use.
type User struct {
	OpenID string `json:"open_id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// Mention is an @ entry attached to a raw Feishu message.
type Mention struct {
	Key       string `json:"key"`
	ID        string `json:"id"`
	IDType    string `json:"id_type,omitempty"`
	Name      string `json:"name"`
	TenantKey string `json:"tenant_key,omitempty"`
}

// RawMessage mirrors the Feishu im/v1/messages item.
type RawMessage struct {
	MessageID  string `json:"message_id"`
	RootID     string `json:"root_id,omitempty"`
	ParentID   string `json:"parent_id,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	MsgType    string `json:"msg_type"`
	CreateTime string `json:"create_time"`
	UpdateTime string `json:"update_time,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
	Updated    bool   `json:"updated,omitempty"`
	ChatID     string `json:"chat_id,omitempty"`
	Sender     struct {
		ID         string `json:"id"`
		IDType     string `json:"id_type"`
		SenderType string `json:"sender_type"`
		TenantKey  string `json:"tenant_key,omitempty"`
	} `json:"sender"`
	Body struct {
		Content string `json:"content"`
	} `json:"body"`
	Mentions       []Mention `json:"mentions,omitempty"`
	UpperMessageID string    `json:"upper_message_id,omitempty"`
}

// Node is one inline or block element of the render model.
type Node struct {
	Type   string     `json:"type"` // text|link|at|image|media|file|emoji|code|hr|table
	Text   string     `json:"text,omitempty"`
	Href   string     `json:"href,omitempty"`
	Style  []string   `json:"style,omitempty"`
	Key    string     `json:"key,omitempty"`   // image_key / file_key
	Cover  string     `json:"cover,omitempty"` // media cover image_key
	UserID string     `json:"user_id,omitempty"`
	Lang   string     `json:"lang,omitempty"`
	IsDoc  bool       `json:"is_doc,omitempty"`
	Block  string     `json:"block,omitempty"`  // h1..h6 | sec | ul | ol | quote
	Indent int        `json:"indent,omitempty"` // list nesting level (0, 1, 2...)
	Marker string     `json:"marker,omitempty"` // list marker ("•", "1.", etc.)
	Cells  [][][]Node `json:"cells,omitempty"`  // for type=="table": rows -> cols -> inline nodes (row 0 = header)
	// NoDL marks images/files whose bytes can never be fetched by this app
	// (resources inside cards or merged-forward messages: API error 234043).
	NoDL bool `json:"no_dl,omitempty"`
}

// Attachment is a file / image / link involved in a topic.
type Attachment struct {
	LastReferenced int64  `json:"last_referenced,omitempty"`
	ResourceType   string `json:"resource_type,omitempty"`
	ResourceToken  string `json:"resource_token,omitempty"`
	SourceFolder   string `json:"source_folder,omitempty"`
	SourcePath     string `json:"source_path,omitempty"`
	ResolveError   string `json:"resolve_error,omitempty"`
	RemoteModified int64  `json:"remote_modified,omitempty"`
	ID             string `json:"id"`
	Kind           string `json:"kind"` // image|file|audio|video|doc|link
	Name           string `json:"name"`
	MessageID      string `json:"message_id,omitempty"`
	FileKey        string `json:"file_key,omitempty"`
	ResType        string `json:"res_type,omitempty"` // image|file (Feishu resource type)
	CoverKey       string `json:"cover_key,omitempty"`
	URL            string `json:"url,omitempty"`
	SenderName     string `json:"sender_name,omitempty"`
	Time           int64  `json:"time,omitempty"`
	// NoDownload: the resource lives in a card / merged-forward message, which
	// the message-resource API refuses (234043), so no preview is attempted.
	NoDownload bool `json:"no_download,omitempty"`
}

// Card is a rendered interactive card (header + block tree).
type Card struct {
	Title    string      `json:"title,omitempty"`
	Subtitle string      `json:"subtitle,omitempty"`
	Template string      `json:"template,omitempty"` // header colour name
	Tags     []string    `json:"tags,omitempty"`
	Blocks   []CardBlock `json:"blocks"`
}

// CardButton is a button-like card component (only url buttons are clickable).
type CardButton struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	Kind string `json:"kind,omitempty"` // primary|danger|default|select
}

// CardBlock is one component of a card.
//
// Type: text | hr | note | image | actions | columns | collapse | group | table | fields
type CardBlock struct {
	Type     string        `json:"type"`
	Paras    [][]Node      `json:"paras,omitempty"`    // text, note
	Children []CardBlock   `json:"children,omitempty"` // collapse, group, fields
	Cols     [][]CardBlock `json:"cols,omitempty"`     // columns
	Weights  []int         `json:"weights,omitempty"`  // columns
	Title    string        `json:"title,omitempty"`    // collapse header, image alt
	Open     bool          `json:"open,omitempty"`     // collapse expanded
	Short    bool          `json:"short,omitempty"`    // fields: half-width cell
	Buttons  []CardButton  `json:"buttons,omitempty"`
	Key      string        `json:"key,omitempty"` // image key
	Header   []string      `json:"header,omitempty"`
	Rows     [][]string    `json:"rows,omitempty"`
}

// Content is the parsed render model of a message body.
type Content struct {
	Title       string       `json:"title,omitempty"`
	Paragraphs  [][]Node     `json:"paragraphs"`
	Plain       string       `json:"plain"`
	Card        *Card        `json:"card,omitempty"`
	Notice      string       `json:"notice,omitempty"`
	Attachments []Attachment `json:"-"`
}

// Message is a rendered message ready for the UI.
type Message struct {
	ID           string   `json:"id"`
	ParentID     string   `json:"parent_id,omitempty"`
	SenderID     string   `json:"sender_id"`
	SenderName   string   `json:"sender_name"`
	SenderAvatar string   `json:"sender_avatar,omitempty"`
	SenderType   string   `json:"sender_type"`
	Time         int64    `json:"time"`
	MsgType      string   `json:"msg_type"`
	Title        string   `json:"title,omitempty"`
	Paragraphs   [][]Node `json:"paragraphs"`
	Plain        string   `json:"plain"`
	Card         *Card    `json:"card,omitempty"`
	// Notice is a machine-readable reason a body could not be shown:
	// card_unavailable | forward_failed.
	Notice    string    `json:"notice,omitempty"`
	Forwarded []Message `json:"forwarded,omitempty"` // merge_forward children
	Deleted   bool      `json:"deleted,omitempty"`
	Edited    bool      `json:"edited,omitempty"`
	IsRoot    bool      `json:"is_root,omitempty"`
}

// Topic is a thread summary for the left column.
type Topic struct {
	ThreadID        string `json:"thread_id"`
	ChatID          string `json:"chat_id"`
	ChatName        string `json:"chat_name"`
	RootMessageID   string `json:"root_message_id"`
	Title           string `json:"title"`
	Snippet         string `json:"snippet,omitempty"`
	SenderID        string `json:"sender_id"`
	SenderName      string `json:"sender_name"`
	SenderAvatar    string `json:"sender_avatar,omitempty"`
	SenderType      string `json:"sender_type,omitempty"`
	CreateTime      int64  `json:"create_time"`
	LastTime        int64  `json:"last_time"`
	ReplyCount      int    `json:"reply_count"` // -1 = unknown
	AttachmentCount int    `json:"attachment_count"`
	// Pending: a root message the user just sent that has no thread yet
	// (ThreadID is then "new_<message_id>"); it becomes a real thread with the
	// first reply sent into it.
	Pending bool `json:"pending,omitempty"`
	// UserIDs lists all unique user IDs that participated in or were mentioned in this topic.
	UserIDs []string `json:"user_ids,omitempty"`
	// UserInvolved reports whether the currently logged-in user is involved in this topic.
	UserInvolved bool `json:"user_involved,omitempty"`
}

// TopicsResp is the response of GET /api/topics.
type TopicsResp struct {
	ChatID   string   `json:"chat_id,omitempty"`
	Topics   []Topic  `json:"topics"`
	Warnings []string `json:"warnings,omitempty"`
}

// ThreadResp is the response of GET /api/topics/{id}/messages.
type ThreadResp struct {
	// OpenURL opens the chat in the Feishu client (AppLink). Feishu offers no
	// public way to build a link to a single message or topic.
	OpenURL     string       `json:"open_url,omitempty"`
	Topic       Topic        `json:"topic"`
	Messages    []Message    `json:"messages"`
	Attachments []Attachment `json:"attachments"`
	Warnings    []string     `json:"warnings,omitempty"`
	// Downloads is the local download state of the conversation's files.
	Downloads []DLEntry     `json:"downloads"`
	FileSync  TopicFileSync `json:"file_sync"`
	// BotBusy and BotStatus report whether Daidainiao is currently running a task in this topic.
	BotBusy   bool         `json:"bot_busy"`
	BotStatus string       `json:"bot_status,omitempty"`
	Queue     []QueuedItem `json:"queue,omitempty"`
}
