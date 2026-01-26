# Easy-Agent

一个基于 Ollama 的 AI 编程助手代理服务，提供代码审查、沙箱执行、文件操作、RAG（检索增强生成）等能力。

详细的内容介绍全在微信公众号中。干货持续更新，敬请关注「代码扳手」微信公众号：

<img width="430" height="430" alt="image" src="wx.jpg" />

## 核心功能
- 🤖 **AI 编程助手**：通过 Ollama 接入大语言模型，提供代码审查、测试生成、修复建议。
- 🛡️ **安全沙箱**：支持在隔离的 Docker 环境中运行 Python/Go 代码（`run_code` 工具）。
- 📁 **文件操作**：安全读写文件（`read_file`/`write_file`），支持路径白名单和大小限制。
- 🌿 **Git 集成**：执行安全的 Git 操作（`git_cmd`），支持白名单命令。
- 🧠 **RAG 知识库**：支持上传文件（`.txt`, `.md`, `.pdf`）构建本地向量知识库，增强 AI 回答的准确性。
- 📡 **多模式 API**：
  - RESTful API (`/agent`, `/session`, `/upload`)
  - SSE 流式响应 (`/stream`)
  - WebSocket 实时通信 (`/ws`)
- 📊 **可观测性**：集成 OpenTelemetry 用于链路追踪。

## 快速启动

### 前置要求
1. **Go 1.24+**
2. **Docker** (用于代码沙箱)
3. **Ollama** (运行大语言模型)

### 1. 启动 Ollama
确保 Ollama 已安装并运行，且已下载所需的模型（默认使用 `qwen2.5-coder:3b` 和 `nomic-embed-text`）。
```bash
ollama pull qwen2.5-coder:3b
ollama pull nomic-embed-text
```

### 2. 配置应用
项目根目录下包含默认配置，您也可以创建 `config.yaml` 进行自定义：

```yaml
server:
  address: ":8080"
  static_path: "./client"

ollama:
  url: "http://localhost:11434/api/chat"
  default_model: "qwen2.5-coder:3b"
  timeout_secs: 300

storage:
  memory_path: "./memory_store"
  vector_path: "./memory_store"

agent:
  max_iterations: 6

sandbox:
  max_concurrency: 5
  memory_mb: 256
  cpu_quota: 0.5
```

### 3. 运行服务
```bash
# 运行代理服务
go run main.go
```

服务启动后，访问 `http://localhost:8080` 即可使用内置的 Web 客户端。

## API 使用示例

### 1. RESTful 调用
```bash
curl -X POST http://localhost:8080/agent \
  -H "Content-Type: application/json" \
  -d '{"prompt": "用 Go 写一个快速排序"}'
```

### 2. SSE 流式响应
```bash
curl "http://localhost:8080/stream?prompt=用Go写斐波那契数列"
```

### 3. 文件上传 (RAG)
```bash
curl -X POST http://localhost:8080/upload \
  -F "file=@./README.md"
```

## 架构说明
```
agent/
├── agent_loop.go    # 核心代理循环 (ReAct 模式)
├── config.go        # 配置管理
├── knowledge.go     # RAG 知识库实现
├── memory_v3.go     # 会话记忆管理
├── ollama_client.go # Ollama API 客户端
├── tools.go         # 工具实现 (沙箱/文件/Git)
├── tool_registry.go # 工具注册表
└── tracer.go        # OpenTelemetry 追踪
web/
├── handlers.go      # HTTP 接口处理
├── routes.go        # 路由定义
└── ws_handler.go    # WebSocket 处理
prompts/             # 提示词模板
sandboxes/           # 沙箱临时目录
```

## 安全特性
1. **代码沙箱**：
   - 基于 Docker 容器隔离
   - 禁用网络访问
   - 资源限制（CPU/内存）
   - 自动清理临时文件
2. **文件操作限制**：
   - 读取文件大小限制
   - 写入需明确指定路径
   - 上传文件类型白名单验证
   - 文件名路径遍历防御
3. **Git 命令白名单**：仅允许安全的只读或状态查询命令。
4. **工具确认机制**：敏感操作（如代码执行、文件写入）支持需用户确认。
5. **CORS 支持**：配置了跨域资源共享策略。

## 开发说明
1. **添加新工具**：
   - 在 `agent/tools.go` 中实现 `Tool` 接口。
   - 在 `agent/agent_loop.go` 的 `registerDefaultTools` 方法中注册。
2. **修改配置**：
   - 修改 `config.yaml` 或设置环境变量（前缀 `EASYAGENT_`，如 `EASYAGENT_SERVER_ADDRESS`）。
3. **日志**：
   - 日志文件位于 `logs/app.log` (JSON 格式)。
   - 控制台输出人类可读的日志。

## 许可证
MIT
