package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/text"
)

// chatCompletionChunk is a single SSE payload of the streaming response (OpenAI chat.completion.chunk).
// tool_calls is index-based incremental: the first chunk carries id/name, subsequent chunks only carry argument fragments.
type chatCompletionChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error: some providers/gateways report mid-stream errors via {"error":{"message":...}} chunk (content filtering,
	// upstream failures). The standard chat.completion.chunk schema has no such field; its presence is treated as a stream-level error (P1-3).
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Delta type lives in core (miniagent.Delta, referenced by the LLM.DoStream contract); this package uses it via miniagent.Delta.

// streamToolCall accumulates fragments of a single tool_call (routed by delta.index).
type streamToolCall struct {
	id, name string
	args     strings.Builder
}

// maxStreamResponseBytes limits the number of bytes accumulated by a single streaming response, preventing OOM from malicious/abnormal providers streaming indefinitely.
const maxStreamResponseBytes = 4 << 20 // 4 MiB

// maxErrorChunkChars truncates the message field of a provider error chunk, preventing a malicious proxy from echoing
// credentials/oversized text in the error message (aligned with the non-200 path policy of not echoing the body).
const maxErrorChunkChars = 256

// streamAccum aggregates multiple chunks into a complete Response.
type streamAccum struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      map[int]*streamToolCall // key = delta.index
	callOrder  []int                   // index appearance order, for stable aggregation
	finish     string
	usage      miniagent.Usage
	sawChoice  bool // whether a chunk with choices was seen (empty-response detection, P1-2)
	doneSeen   bool // whether the terminal "data: [DONE]" marker was seen (connection-drop detection)
	totalBytes int  // accumulated bytes upper-bound guard
}

// errStreamUnterminated is returned (alongside the partial Response) when content/reasoning/tool_calls were received but
// the stream ended without EITHER the [DONE] marker or a finish_reason — a connection drop that would otherwise masquerade
// as a silent partial success. StreamClient.DoStream relaxes it to success when the provider opted in via
// StreamAllowUnterminated (non-compliant endpoints such as vLLM/Ollama that emit content then close with no terminator).
var errStreamUnterminated = errors.New("stream ended without [DONE]/finish_reason (connection dropped mid-generation)")

// parseSSE reads the SSE stream: for each content/reasoning fragment it calls onDelta; when onDelta returns an error it aborts; on completion it returns the aggregated Response.
// Line format: starts with "data: ", payload is JSON; "data: [DONE]" terminates; blank lines/":" comments are ignored.
func parseSSE(r io.Reader, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	acc := &streamAccum{calls: map[int]*streamToolCall{}}
	sc := bufio.NewScanner(r)
	// Relax the default 64KB line length to 4MB (same tier as maxChatBodyBytes), to avoid ErrTooLong triggered by oversized single-line chunks (P3-2).
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		// Strip leading UTF-8 BOM: a few proxies/gateways insert a BOM at the start of the SSE stream; otherwise the first line "data: ..." would not match HasPrefix("data:") and the first event would be dropped.
		line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue // empty data line (proxy keepalive), does not interrupt the stream
		}
		if data == "[DONE]" {
			acc.doneSeen = true
			break
		}
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return miniagent.Response{}, fmt.Errorf("parse sse chunk: %w", err)
		}
		// provider/gateway reports an error via error chunk (content filtering/upstream failure): propagate it rather than swallowing it as success (P1-3).
		// Truncate the message: aligned with the non-200 path policy of not echoing the body (prevents malicious proxies from echoing credentials/oversized text in the error message).
		if chunk.Error != nil {
			return miniagent.Response{}, fmt.Errorf("stream error from provider: %s", text.Truncate(chunk.Error.Message, maxErrorChunkChars, "…"))
		}
		if err := acc.apply(chunk, onDelta); err != nil {
			return miniagent.Response{}, err
		}
	}
	if err := sc.Err(); err != nil {
		return miniagent.Response{}, fmt.Errorf("read sse: %w", err)
	}
	res := acc.response()
	// Never saw choices (truncated/empty stream/usage-only): report an error rather than masquerading as an empty answer, aligned with parseChatResponse (P1-2).
	if !acc.sawChoice && res.FinishReason == "" && res.Text == "" && len(res.ToolCalls) == 0 {
		return miniagent.Response{}, errors.New("stream ended without any choices")
	}
	// Connection-drop detection: content/reasoning/tool_calls were received but the stream ended with NEITHER the
	// [DONE] marker NOR a finish_reason — a reverse proxy/LB closing the connection mid-generation is otherwise
	// indistinguishable from a well-terminated stream (clean EOF, sc.Err()==nil), and the partial content would be
	// returned as a silent success (loop.go treats it as FinishStop). The OR of "no [DONE]" AND "no finish_reason" is
	// intentional: spec-compliant providers signal completion via EITHER marker (Azure / some LiteLLM configs emit
	// finish_reason but never [DONE]), so a finish_reason-only stream still succeeds.
	if !acc.doneSeen && res.FinishReason == "" && (acc.text.Len() > 0 || acc.reasoning.Len() > 0 || len(acc.callOrder) > 0) {
		// Return the partial Response (not empty) so DoStream can still hand the caller the content it received when the
		// provider opted into StreamAllowUnterminated.
		return res, errStreamUnterminated
	}
	return res, nil
}

func (a *streamAccum) apply(ch chatCompletionChunk, onDelta func(miniagent.Delta) error) error {
	// usage typically appears only in the final chunk (stream_options.include_usage), without choices.
	if ch.Usage != nil {
		a.usage = miniagent.Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	}
	if len(ch.Choices) == 0 {
		return nil
	}
	a.sawChoice = true
	c := ch.Choices[0]
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Delta.Content != "" {
		if err := a.guardAdd(len(c.Delta.Content)); err != nil {
			return err
		}
		a.text.WriteString(c.Delta.Content)
		if onDelta != nil {
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaText, Text: c.Delta.Content}); err != nil {
				return err
			}
		}
	}
	// reasoning dual-compat: DeepSeek reasoning_content, others reasoning; the former takes precedence.
	rc := c.Delta.ReasoningContent
	if rc == "" {
		rc = c.Delta.Reasoning
	}
	if rc != "" {
		if err := a.guardAdd(len(rc)); err != nil {
			return err
		}
		a.reasoning.WriteString(rc)
		if onDelta != nil {
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaReasoning, Text: rc}); err != nil {
				return err
			}
		}
	}
	for _, tc := range c.Delta.ToolCalls {
		acc, ok := a.calls[tc.Index]
		if ok && acc.id != "" && tc.ID != "" && tc.ID != acc.id {
			// The same index has already merged tool_calls with different ids: the provider omitted index, causing multiple tool_calls to be incorrectly merged.
			// When index is missing, subsequent fragments cannot be reliably routed; report an error explicitly rather than silently merging (native OpenAI always carries index, only compat endpoints omit it).
			return fmt.Errorf("streaming tool_call missing index: %q and %q merged into index %d", acc.id, tc.ID, tc.Index)
		}
		// Index collision without ids: a name-bearing fragment at an existing index whose name DIFFERS means a distinct
		// tool_call is colliding into this index — compat endpoints that omit index (every call at index 0) with no stable
		// ids would otherwise silently overwrite the name and concatenate args. Spec-compliant streams carry the name only
		// on the first fragment of an index, so a second distinct name is a reliable collision signal (zero false positives).
		if ok && acc.name != "" && tc.Function.Name != "" && tc.Function.Name != acc.name {
			return fmt.Errorf("streaming tool_call index collision: %q and %q merged into index %d (provider omitted index)", acc.name, tc.Function.Name, tc.Index)
		}
		if !ok {
			acc = &streamToolCall{}
			a.calls[tc.Index] = acc
			a.callOrder = append(a.callOrder, tc.Index)
		}
		if tc.ID != "" {
			if err := a.guardAdd(len(tc.ID)); err != nil {
				return err
			}
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			if err := a.guardAdd(len(tc.Function.Name)); err != nil {
				return err
			}
			acc.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			if err := a.guardAdd(len(tc.Function.Arguments)); err != nil {
				return err
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
	return nil
}

// guardAdd checks whether adding n bytes would exceed the single-response upper bound.
func (a *streamAccum) guardAdd(n int) error {
	if a.totalBytes+n > maxStreamResponseBytes {
		return fmt.Errorf("streaming response accumulated exceeds %d byte limit", maxStreamResponseBytes)
	}
	a.totalBytes += n
	return nil
}

// response aggregates tool_calls in ascending index order, consistent with the handleToolCalls ordering contract.
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
