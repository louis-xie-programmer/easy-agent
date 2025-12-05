# 客户端使用示例

本目录包含调用Golang-Agent服务的客户端代码示例，演示了三种API接口的使用方法。

## 示例文件说明

### `client_example.go`

一个完整的Go程序，展示了如何通过以下三种方式与代理服务交互：

1. **RESTful API** (`/agent` 端点)
   - 使用标准HTTP POST请求
   - 接收JSON格式的完整响应
   - 适用于短小、确定性的请求

2. **SSE流式接口** (`/stream` 端点)
   - 建立长连接接收服务器发送事件
   - 支持心跳机制保持连接活跃
   - 适用于需要实时反馈的长文本生成

3. **WebSocket实时通信** (`/ws` 端点)
   - 双向实时通信
   - 流式传输AI生成内容
   - 提供最佳用户体验

### `index.html`

一个现代化的HTML聊天界面，提供：
- 响应式设计，适配移动设备
- 实时消息流显示
- 思考状态指示器（动画效果）
- 连接状态监控
- 键盘快捷键支持（Enter发送）

## 运行前提

确保已启动以下服务：
```bash
# 1. 启动Ollama服务
docker run -d -p 11434:11434 --name ollama ollama/ollama

# 2. 启动代理服务
go run main.go
```

## 运行示例

### 1. Go客户端示例
```bash
# 在项目根目录执行
cd client
go run client_example.go
```

### 2. HTML聊天界面
```bash
# 方法一：直接打开index.html文件
# 在浏览器中打开 client/index.html

# 方法二：使用Python快速启动HTTP服务
python -m http.server 8000
# 然后访问 http://localhost:8000/client/
```

## 关键特性
- 完整的错误处理
- 超时和连接管理
- SSE事件解析器
- 清晰的注释说明

> 提示：可修改示例中的prompt参数来测试不同场景