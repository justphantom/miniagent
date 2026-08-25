package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/justphantom/miniagent/miniagent"
)

// SessionSummary 是 GET /api/sessions 的列表项，与 minisession 服务端定义一致。
type SessionSummary struct {
	ID           string `json:"id"`
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Created      string `json:"created,omitempty"`
	Size         int64  `json:"size"`
	Modified     string `json:"modified"`
	Preview      string `json:"preview"`
	MessageCount int    `json:"message_count"`
}

// Client 通过 HTTP 访问 minisession 服务，替代本地文件 I/O，
// 使 miniagent 可在多机器间接续同一会话。JSONL 编解码复用本地实现
// 的类型定义（SessionMeta/miniagent.Message），格式完全互操作。
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// NewClient 创建 minisession 客户端。baseURL 形如 "http://127.0.0.1:9797"。
func NewClient(baseURL, key string) *Client {
	return &Client{
		baseURL: baseURL,
		key:     key,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateSession 创建会话（POST /api/sessions），返回服务端补全的 meta（id/created）。
func (c *Client) CreateSession(ctx context.Context, meta SessionMeta) (SessionMeta, error) {
	body, err := json.Marshal(map[string]string{
		"id":       meta.ID,
		"model":    meta.Model,
		"provider": meta.Provider,
		"workdir":  meta.Workdir,
	})
	if err != nil {
		return SessionMeta{}, err
	}
	var got SessionMeta
	if err := c.do(ctx, http.MethodPost, "/api/sessions", body, http.StatusCreated, &got); err != nil {
		return SessionMeta{}, err
	}
	return got, nil
}

// LoadSession 读取会话（GET /api/sessions/{id} 与 /messages）。
// 不存在时返回包装 os.ErrNotExist 的错误，与本地文件语义对齐。
// messages 端点单页上限 1000（服务端硬顶），必须按 offset 翻页取全量：
// 只取首页会把超过 1000 条的会话静默截尾，接续后的全量 Rewrite 将使丢失永久化。
func (c *Client) LoadSession(ctx context.Context, id string) (SessionMeta, []miniagent.Message, error) {
	if err := ValidateSessionID(id); err != nil {
		return SessionMeta{}, nil, err
	}
	var meta SessionMeta
	if err := c.do(ctx, http.MethodGet, "/api/sessions/"+id, nil, http.StatusOK, &meta); err != nil {
		return SessionMeta{}, nil, err
	}
	const page = 1000
	var msgs []miniagent.Message
	for offset := 0; ; offset += page {
		var part []miniagent.Message
		path := fmt.Sprintf("/api/sessions/%s/messages?limit=%d&offset=%d", id, page, offset)
		if err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &part); err != nil {
			return SessionMeta{}, nil, err
		}
		msgs = append(msgs, part...)
		if len(part) < page {
			return meta, msgs, nil
		}
	}
}

// AppendMessages 追加消息（POST /api/sessions/{id}/messages）。
func (c *Client) AppendMessages(ctx context.Context, id string, msgs []miniagent.Message) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	//nolint:musttag // 内联请求体，Messages 已带 json tag；musttag 对内联匿名 struct 误报
	body, err := json.Marshal(struct {
		Messages []miniagent.Message `json:"messages"`
	}{msgs})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/api/sessions/"+id+"/messages", body, http.StatusOK, nil)
}

// RewriteMessages 全量覆写（PUT /api/sessions/{id}/messages）。
func (c *Client) RewriteMessages(ctx context.Context, id string, meta SessionMeta, msgs []miniagent.Message) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"meta":     meta,
		"messages": msgs,
	})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/api/sessions/"+id+"/messages", body, http.StatusOK, nil)
}

// DeleteSession 删除会话（DELETE /api/sessions/{id}）。
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/api/sessions/"+id, nil, http.StatusOK, nil)
}

// ListSessions 返回会话摘要列表（跨机器接续选择会话场景）。
func (c *Client) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	var out []SessionSummary
	err := c.do(ctx, http.MethodGet, "/api/sessions", nil, http.StatusOK, &out)
	return out, err
}

// do 发送请求并校验状态码；404 统一包装 os.ErrNotExist 保持语义兼容。
func (c *Client) do(ctx context.Context, method, path string, body []byte, wantStatus int, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// 读上限与本地会话尺寸上限（maxSessionBytes，默认 50MB）对齐加信封余量：迁入的存量
	// 会话可接近该尺寸，1MB 级上限会在 JSON 半途截断，unmarshal 报出误导性的错误。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionBytes+(1<<20)))
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("minisession: %s: %w", path, os.ErrNotExist)
		}
		return fmt.Errorf("minisession: %d %s: %s", resp.StatusCode, path, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
