package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"text/template"
	"time"
)

// PromptManager 管理提示词模板
type PromptManager struct {
	promptsDir string
	templates  map[string]*template.Template
}

// NewPromptManager 创建新的提示词管理器
func NewPromptManager(dir string) *PromptManager {
	if dir == "" {
		dir = "./prompts"
	}
	return &PromptManager{
		promptsDir: dir,
		templates:  make(map[string]*template.Template),
	}
}

// Load 加载指定名称的提示词模板
func (pm *PromptManager) Load(name string) error {
	path := filepath.Join(pm.promptsDir, name+".txt")
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return err
	}

	pm.templates[name] = tmpl
	return nil
}

// Render 渲染提示词
func (pm *PromptManager) Render(name string, data any) (string, error) {
	tmpl, ok := pm.templates[name]
	if !ok {
		// 尝试按需加载
		if err := pm.Load(name); err != nil {
			return "", err
		}
		tmpl = pm.templates[name]
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// DefaultSystemPromptData 默认系统提示词的数据上下文
type DefaultSystemPromptData struct {
	Time string
}

// GetSystemPrompt 获取渲染后的系统提示词
func (pm *PromptManager) GetSystemPrompt() string {
	data := DefaultSystemPromptData{
		Time: time.Now().Format("2006-01-02 15:04:05"),
	}

	prompt, err := pm.Render("system_default", data)
	if err != nil {
		// 回退到硬编码的默认值，防止文件丢失导致崩溃
		Logger.Error().Err(err).Msg("Failed to render system prompt")
		return "你是严格遵守规则的AI助手。当前时间：" + data.Time
	}
	return prompt
}
