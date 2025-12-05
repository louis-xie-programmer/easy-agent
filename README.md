# Easy-Agent

一个基于 Ollama 的 AI 编程助手代理服务，提供代码审查、沙箱执行、文件操作等能力。

详细的内容介绍全在微信公众号中。干货持续更新，敬请关注「代码扳手」微信公众号：

<img width="430" height="430" alt="image" src="wx.jpg" />

## 核心功能
- 🤖 **AI 编程助手**：通过 Ollama 接入大语言模型，提供代码审查、测试生成、修复建议
- 🛡️ **安全沙箱**：支持在隔离环境中运行 Python/Go 代码（`run_code` 工具）
- 📁 **文件操作**：安全读写文件（`read_file`/`write_file`）
- 🌿 **Git 集成**：执行安全的 Git 操作（`git_cmd`）
- 📡 **双模式 API**：
  - RESTful API (`/agent`)
  - SSE 流式响应 (`/stream`)

## 快速启动
```bash
# 运行代理服务
go run main.go
```

## 配置参数
| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama 服务地址 |
| `AGENT_ADDR` | `:8080` | 代理服务监听地址 |

## API 使用示例
### 1. RESTful 调用
```bash
curl -X POST http://localhost:8080/agent \
  -H "Content-Type: application/json" \
  -d '{"prompt": "用 Go 写一个快速排序"}'
```

### 2. SSE 流式响应
```bash
curl http://localhost:8080/stream?prompt=用Go写斐波那契数列
```

### 3. 浏览器直接访问(ws)

https://localhost:8080/

## 架构说明
```
agent/
├── agent_loop.go    # 核心代理循环
├── memory.go        # 会话记忆管理
├── ollama_client.go # Ollama API 客户端
└── tools.go         # 安全工具集（沙箱/文件/Git）
web/
└── handlers.go      # HTTP 接口处理
```



## 安全特性
1. 代码沙箱限制：
   - 禁用网络访问
   - CPU/内存限制（256MB/0.5核）
   - 进程数限制（64）
2. 文件操作限制：
   - 读取文件大小限制（20KB）
   - 写入需明确指定路径
3. Git 命令白名单机制

## 开发说明
1. 工具函数注册位置：`agent_loop.go` 中的 `toolsMetadata()`
2. 内存持久化：`agent_memory.json` 文件
3. 日志路径：标准输出（需配合日志收集系统）

