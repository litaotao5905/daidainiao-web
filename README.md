# 网页呆呆鸟 (Daidainiao Web) v1.9.11

以本人飞书账号浏览话题、回复消息，并管理呆呆鸟对话中的在线文档和本地文件。支持 Windows WebView2 桌面窗口、现代浏览器（PWA）和 Docker 容器化运行。

## 最新特性 (v1.9.11)

- **话题阅读进度与未读标记**：
  - 自动记录每个话题的已读游标。首次加载默认将历史话题平滑置为已读；
  - 当群聊或话题有新消息或新回复时，左侧话题列表自动显示 **🔴 未读红点** 与 **「新回复」** 徽标，标题高亮；
  - 点击会话阅读即刻消除已读；顶部工具栏提供「✓ 全部标记为已读」快捷按钮；已读状态在服务端原子化持久存储，跨浏览器、PWA 及桌面端实时同步。
- **附件私密加密与权限弹窗**：
  - 发送消息携带附件时，弹窗提供「🔒 加密发送（仅本人和呆呆鸟可读）」与「👥 群可阅读」权限模式选择；
  - 彻底隔离快捷键冒泡，支持键盘 Enter/左右方向键/Esc 键盘无障碍操作。
- **资料按需下载与导出**：
  - 支持飞书云文档直接导出为 Word (.docx)、Excel (.xlsx)、PowerPoint (.pptx)；
  - 文件夹支持按需递归分页展开；未下载文件后台异步安全补全标题。
- **多端与自动化提醒**：
  - 支持任务完成飞书单聊提醒、浏览器桌面通知与音效提醒；
  - 兼容 macOS（Apple Silicon & Intel 通用）、Windows x64 绿色免安装 GUI 以及 Linux Docker / OrbStack 一键部署。

## 快速运行

需要 Go 1.22 或以上。复制 `config.example.json` 为 `config.json`，填写自己的应用 App ID 与 App Secret，保留 `data_dir` 为本机目录。飞书开放平台配置回调 `http://localhost:8088/callback`。

```sh
go run . -server
```

浏览器打开 `http://localhost:8088`，用本人账号登录。Windows 绿色包解压后先配置或复制本人原来的 `config.json`，再双击 `网页呆呆鸟.exe`。首次使用 WebView2 的 Windows 机器需具备 Edge WebView2 Runtime。

升级前退出旧程序并备份原来的 `config.json`、`data` 和下载目录。新包不含真实凭据或历史数据；在原目录替换程序时保留个人配置与数据。配置中新增权限后，需要在开放平台开通、发布版本，并在界面重新登录取得授权。

## 配置与权限

- `upload_mode`：`off` 禁止上传；旧的 `auto`、`im`、`drive` 均统一为私密云空间上传，不能通过 IM 绕过权限控制。
- `drive_share_mode`：旧字段兼容读取，统一使用 `bot_only`，不能用配置把全部附件改成群可读。是否授予当前群阅读权限由发送时的「加密附件」选项决定，默认加密。
- `bot_open_ids`：可选，仅兼容配置项目原有红色呆呆鸟的单一 `ou_` 身份；留空使用默认目标。授权结果会验证为应用协作者，不能配置普通用户或群聊。
- `download_dir`：默认系统「文档」中的 `FeishuTopics`。手动下载及导出文件保存在 `<群名>/<话题>/<来源目录>/<文件名>`；直接附件没有来源目录层。
- `host_download_dir`：Docker 映射的宿主机目录；保留现有宿主机打开助手支持。
- `disable_download`：保留原有关闭文件下载选项。

除聊天、联系人、发送消息所需权限外，附件及资料同步使用以下用户授权：

| 权限 | 用途 |
| --- | --- |
| `drive:file:upload` | 上传附件 |
| `drive:file:download` | 下载普通文件 |
| `drive:drive.metadata:readonly` | 查询文件名、所有者、修改时间和访问链接 |
| `space:document:retrieve` | 列举文件夹内容 |
| `space:folder:create` | 新建私密附件目录 |
| `docs:permission.member:create` | 授予呆呆鸟阅读权限；取消加密时同时授予当前群阅读权限 |
| `docs:permission.member:retrieve` | 读取协作者，验证应用身份及授权范围 |
| `docs:permission.setting:read` | 读取文件和父目录分享设置 |
| `docs:permission.setting:write_only` | 关闭新附件的链接分享，限制协作者管理 |
| `wiki:node:read` | 手动下载时解析 Wiki 对应的实际资源 |
| `docs:document:export` | 手动导出在线文档、表格及幻灯片 |

新增在线导出需要在飞书开放平台开通并发布 `docs:document:export`，已有登录需重新授权。程序自动补齐这些窄范围权限；已有 `drive:drive` 授权的配置继续兼容。消息资源下载沿用应用身份及用户身份的原有逻辑。

## 界面和接口

在线文档和文件页签显示本地下载状态，不代表云端版本是否最新。未下载的图片使用占位显示，点击下载后才加载本地预览。材料按消息时间从新到旧展示，来源文件夹折叠保留入口。

主要接口沿用原路径：

| 接口 | 行为 |
| --- | --- |
| `GET /api/topics/{thread_id}/messages` | 消息、附件目录及本地下载状态；名称缺失或链接被新消息再次引用时后台查询标题，不检查云端版本。`refresh_names=1` 显式刷新当前话题标题；`refresh=1` 只刷新消息，旧的 `sync_files=1` 不再触发同步 |
| `GET /api/downloads?topic=<root_id>` | 本地文件列表和下载进度，不检查云端版本 |
| `POST /api/files/download` | 请求体 `{"id":"文件ID"}`，手动下载、导出或重新下载选中的单个材料；沿用 CSRF 和登录校验 |
| `POST /api/files/redownload` | 兼容原手动重试接口 |
| `POST /api/topics/{thread_id}/materials/expand` | 请求体 `{"token":"当前话题文件夹token"}`，仅展开指定文件夹，不下载子项 |
| `POST /api/topics/{thread_id}/reply` | 不等待资料检查。`encrypt` 省略或为 true 时附件保持私密；`encrypt: false` 时额外授予当前群阅读权限，并在发送前验证 |
| `POST /api/files/open\|reveal` | 沿用内部文件 id、CSRF 和本机访问校验 |

`file_sync.status=manual`；`names_loading=true` 只表示后台标题查询，用本地状态轮询等待显示结果，不代表文件下载或版本同步。下载记录的 `status=not_downloaded` 表示仅登记；`waiting`、`downloading` 仅在手动点击后出现。本地副本存在时为 `done`；丢失时显示未下载，刷新不会自动补下。`downloaded_bytes`、`total_bytes` 保留真实进度，失败保留原因和已有副本。导出资源的 `res_type` 为 `export:<资源类型>`。

## 验证与构建

```sh
gofmt -l .
go vet ./...
go test -race ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-H=windowsgui" -o 网页呆呆鸟.exe .
```

测试涵盖私密上传、权限失败阻断与重试复用、应用身份规范化、Wiki 分类、递归分页、同名文件隔离、手动下载及导出、目录按需展开、刷新不访问云端资料、备份及失败保留、发送及排队不启动扫描、真实字节进度，以及原有鉴权、消息、文件打开和 PWA 功能。

真实飞书测试可通过设置 `DAIDAINIAO_LIVE_CONFIG` 运行 `TestLivePrivateAttachment`；Docker 配置可额外设置 `DAIDAINIAO_LIVE_DATA` 指向宿主机数据目录。该测试创建一次性私密测试附件，不发送群消息，并清理测试附件及本次空目录。授权不足三分钟时会停止，避免主动刷新运行中应用的登录凭据。

## 真实限制与待验收项

- 下载受调用身份可访问范围及飞书限制。卡片、合并转发中的受限原生附件仍展示原因；在线文档可手动导出为受支持的 Office 格式，导出受用户的导出权限和租户策略限制。文件夹快捷方式缺少目标信息时会提示提供目标链接。
- 飞书真实接口已验证私密父目录、关闭分享、仅所有者和指定应用协作、所有者下载及非协作者应用无法下载。群内真实发送后未出现群授权，呆呆鸟通过现有已授权用户身份读取专用附件并回传随机校验码。目标呆呆鸟应用自身凭据读取，以及未授权群成员账号访问，仍需用对应身份完成验收。
- Windows x64 运行包经交叉编译生成；当前环境没有 Windows 真机，桌面启动及系统文件打开仍需真机验收。
- 附件上传最多每轮 9 个、单个 30 MB，大于 20 MB 使用分片上传。
- 本程序继续沿用单用户本地服务架构。源码包与运行包不包含个人配置、Token、缓存或下载资料。

端到端私密发送验收使用 `TestLivePrivateMessage`。必须先取得用户对具体群和测试话题的发送授权，再显式设置 `DAIDAINIAO_LIVE_MESSAGE_CHAT`、`DAIDAINIAO_LIVE_MESSAGE_ROOT` 及上述配置环境变量。测试仅发送无业务内容的专用文件，并清理测试文件及发送的消息。`DAIDAINIAO_LIVE_REQUIRE_BOT_READ=1` 要求呆呆鸟回传仅存在于文件中的随机校验码；如需验证应用自身身份，再设置 `DAIDAINIAO_LIVE_APP_ONLY=1`。该验证需要对应机器人运行环境允许应用身份，不会自动切换其他身份。
