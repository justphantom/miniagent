package anthropic

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// maxStreamResponseBytes caps the bytes accumulated by a single streaming response (mirrors openai).
const maxStreamResponseBytes = 4 << 20 // 4 MiB

// maxErrorChunkChars truncates a provider error message (mirrors openai — never echo credentials/oversized
// text from a malicious or debugging proxy).
const maxErrorChunkChars = 256

// errStreamUnterminated is returned (with the partial Response) when content/thinking/tool_use was received
// but the stream ended without a message_stop event — a connection drop that would otherwise masquerade as a
// silent partial success. StreamClient.DoStream relaxes it to success when the provider opted in via
// StreamAllowUnterminated (non-compliant endpoints).
var errStreamUnterminated = errors.New("anthropic stream ended without message_stop (connection dropped mid-generation)")

// streamToolCall accumulates the input_json_delta fragments of a single tool_use block (routed by index).
type streamToolCall struct {
	id, name string
	args     strings.Builder
}

type streamAccum struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      map[int]*streamToolCall
	callOrder  []int
	finish     string
	usage      miniagent.Usage
	sawContent bool // content_block_start seen
	msgStarted bool // message_start seen
	usageFinal bool // message_delta seen (output_tokens available)
	stopped    bool // message_stop seen
	totalBytes int
}

// parseSSE reads the Anthropic SSE stream: for each text/thinking fragment it calls onDelta; on completion
// it returns the aggregated Response. Each event is two lines (event: <type> then data: {...}); only the
// data: line's JSON "type" drives the state machine (the event: line is redundant and skipped). There is no
// [DONE] marker — message_stop terminates. Lines not prefixed with "data:" (event:/ping comments/keepalives)
// are ignored.
func parseSSE(r io.Reader, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	acc := &streamAccum{calls: map[int]*streamToolCall{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxStreamResponseBytes)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "\xef\xbb\xbf") // strip UTF-8 BOM some proxies prepend
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue // keepalive
		}
		if err := acc.apply([]byte(data), onDelta); err != nil {
			return miniagent.Response{}, err
		}
	}
	if err := sc.Err(); err != nil {
		return miniagent.Response{}, fmt.Errorf("read sse: %w", err)
	}
	return acc.finalize()
}

// eventEnvelope peels off only the "type" discriminator; each branch re-unmarshals the full payload.
type eventEnvelope struct {
	Type string `json:"type"`
}

func (a *streamAccum) apply(data []byte, onDelta func(miniagent.Delta) error) error {
	var env eventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("parse sse event: %w", err)
	}
	switch env.Type {
	case "ping":
		return nil
	case "error":
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("stream error from provider: %s", text.Truncate(e.Error.Message, maxErrorChunkChars, "…"))
	case "message_start":
		a.msgStarted = true
		var ev struct {
			Message struct {
				InputTokens              int `json:"input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
			} `json:"message"`
		}
		_ = json.Unmarshal(data, &ev)
		// Fold cache tokens into InputTokens (core Usage has no cache field; the total input context is
		// what compaction's overflow detection needs).
		a.usage.InputTokens = ev.Message.InputTokens + ev.Message.CacheCreationInputTokens + ev.Message.CacheReadInputTokens
		return nil
	case "content_block_start":
		a.sawContent = true
		var ev struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id,omitempty"`
				Name string `json:"name,omitempty"`
			} `json:"content_block"`
		}
		_ = json.Unmarshal(data, &ev)
		if ev.ContentBlock.Type == "tool_use" {
			if a.calls[ev.Index] == nil {
				a.calls[ev.Index] = &streamToolCall{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				a.callOrder = append(a.callOrder, ev.Index)
			} else {
				a.calls[ev.Index].id = ev.ContentBlock.ID
				a.calls[ev.Index].name = ev.ContentBlock.Name
			}
		}
		return nil
	case "content_block_delta":
		return a.applyDelta(data, onDelta)
	case "content_block_stop":
		return nil
	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason,omitempty"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(data, &ev)
		if ev.Delta.StopReason != "" {
			a.finish = mapStopReason(ev.Delta.StopReason)
		}
		a.usage.OutputTokens = ev.Usage.OutputTokens
		a.usageFinal = true
		return nil
	case "message_stop":
		a.stopped = true
		return nil
	}
	return nil
}

func (a *streamAccum) applyDelta(data []byte, onDelta func(miniagent.Delta) error) error {
	var ev struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			Thinking    string `json:"thinking,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("parse sse delta: %w", err)
	}
	switch ev.Delta.Type {
	case "text_delta":
		if err := a.guardAdd(len(ev.Delta.Text)); err != nil {
			return err
		}
		a.text.WriteString(ev.Delta.Text)
		if onDelta != nil {
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaText, Text: ev.Delta.Text}); err != nil {
				return err
			}
		}
	case "thinking_delta":
		if err := a.guardAdd(len(ev.Delta.Thinking)); err != nil {
			return err
		}
		a.reasoning.WriteString(ev.Delta.Thinking)
		if onDelta != nil {
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaReasoning, Text: ev.Delta.Thinking}); err != nil {
				return err
			}
		}
	case "input_json_delta":
		if c := a.calls[ev.Index]; c != nil {
			if err := a.guardAdd(len(ev.Delta.PartialJSON)); err != nil {
				return err
			}
			c.args.WriteString(ev.Delta.PartialJSON)
		}
	case "signature_delta":
		// ignored — historical thinking is never replayed, so the signature has no consumer
	}
	return nil
}

func (a *streamAccum) guardAdd(n int) error {
	if a.totalBytes+n > maxStreamResponseBytes {
		return fmt.Errorf("streaming response accumulated exceeds %d byte limit", maxStreamResponseBytes)
	}
	a.totalBytes += n
	return nil
}

func (a *streamAccum) finalize() (miniagent.Response, error) {
	// Half-finished usage guard: output_tokens arrive only at message_delta. Exposing {Input:N,Output:0}
	// before that would trigger the compaction zero-usage local-estimation fallback (double counting).
	if !a.usageFinal {
		a.usage = miniagent.Usage{}
	}
	if !a.msgStarted {
		return miniagent.Response{}, errors.New("anthropic stream ended without message_start")
	}
	res := a.response()
	// Connection-drop detection: content/thinking/tool_use was received but message_stop never came — a
	// reverse proxy/LB closing mid-generation is otherwise indistinguishable from a clean stream.
	if !a.stopped && (a.text.Len() > 0 || a.reasoning.Len() > 0 || len(a.callOrder) > 0) {
		return res, errStreamUnterminated
	}
	if !a.sawContent {
		return miniagent.Response{}, errors.New("anthropic stream ended without any content")
	}
	return res, nil
}

func (a *streamAccum) response() miniagent.Response {
	out := miniagent.Response{
		Text:         a.text.String(),
		Reasoning:    a.reasoning.String(),
		FinishReason: a.finish,
		Usage:        a.usage,
	}
	idxs := append([]int(nil), a.callOrder...)
	sort.Ints(idxs)
	for _, i := range idxs {
		tc := a.calls[i]
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{ID: tc.id, Name: tc.name, Args: tc.args.String()})
	}
	return out
}
