package responses

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

const maxStreamResponseBytes = 4 << 20

var errStreamUnterminated = errors.New("responses stream ended without terminal response event")

type streamToolCall struct {
	id, name string
	args     strings.Builder
}

type streamAccum struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      map[int]*streamToolCall
	callOrder  []int
	sawContent bool
	terminal   bool
	final      []byte
	totalBytes int
}

func parseSSE(reader io.Reader, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
	acc := &streamAccum{calls: map[int]*streamToolCall{}}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxStreamResponseBytes)
	for scanner.Scan() {
		line := strings.TrimPrefix(scanner.Text(), "\xef\xbb\xbf")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if err := acc.apply([]byte(data), onDelta); err != nil {
			return miniagent.Response{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return miniagent.Response{}, fmt.Errorf("read sse: %w", err)
	}
	return acc.finalize()
}

func (a *streamAccum) apply(data []byte, onDelta func(miniagent.Delta) error) error {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("parse sse event: %w", err)
	}
	switch env.Type {
	case "error":
		return a.streamError(data)
	case "response.failed":
		var ev struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("parse response.failed: %w", err)
		}
		_, err := parseResponse(ev.Response)
		return err
	case "response.completed", "response.incomplete":
		var ev struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("parse %s: %w", env.Type, err)
		}
		a.final = ev.Response
		a.terminal = true
		return nil
	case "response.output_text.delta":
		return a.addDelta(data, miniagent.DeltaText, &a.text, onDelta)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return a.addDelta(data, miniagent.DeltaReasoning, &a.reasoning, onDelta)
	case "response.function_call_arguments.delta":
		return a.addArguments(data)
	case "response.output_item.added":
		return a.addOutputItem(data)
	default:
		return nil
	}
}

func (a *streamAccum) streamError(data []byte) error {
	var ev struct {
		Error responseError `json:"error"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("parse stream error: %w", err)
	}
	msg := text.Truncate(ev.Error.Message, 256, "…")
	if ev.Error.Code != "" {
		msg = ev.Error.Code + ": " + msg
	}
	return errors.New("stream error from provider: " + msg)
}

func (a *streamAccum) addDelta(data []byte, kind miniagent.DeltaKind, dst *strings.Builder, onDelta func(miniagent.Delta) error) error {
	var ev struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("parse delta: %w", err)
	}
	if ev.Delta == "" {
		return nil
	}
	if err := a.guardAdd(len(ev.Delta)); err != nil {
		return err
	}
	dst.WriteString(ev.Delta)
	a.sawContent = true
	if onDelta != nil {
		if err := onDelta(miniagent.Delta{Kind: kind, Text: ev.Delta}); err != nil {
			return err
		}
	}
	return nil
}

func (a *streamAccum) addArguments(data []byte) error {
	var ev struct {
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("parse function arguments delta: %w", err)
	}
	call := a.calls[ev.OutputIndex]
	if call == nil {
		call = &streamToolCall{}
		a.calls[ev.OutputIndex] = call
		a.callOrder = append(a.callOrder, ev.OutputIndex)
	}
	if err := a.guardAdd(len(ev.Delta)); err != nil {
		return err
	}
	call.args.WriteString(ev.Delta)
	a.sawContent = true
	return nil
}

func (a *streamAccum) addOutputItem(data []byte) error {
	var ev struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("parse output item: %w", err)
	}
	if ev.Item.Type != "function_call" {
		return nil
	}
	call := a.calls[ev.OutputIndex]
	if call == nil {
		call = &streamToolCall{}
		a.calls[ev.OutputIndex] = call
		a.callOrder = append(a.callOrder, ev.OutputIndex)
	}
	call.id, call.name = ev.Item.CallID, ev.Item.Name
	a.sawContent = true
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
	if a.terminal {
		return parseResponse(a.final)
	}
	res := miniagent.Response{Text: a.text.String(), Reasoning: a.reasoning.String()}
	indexes := append([]int(nil), a.callOrder...)
	sort.Ints(indexes)
	for _, index := range indexes {
		call := a.calls[index]
		res.ToolCalls = append(res.ToolCalls, miniagent.ToolCall{ID: call.id, Name: call.name, Args: call.args.String()})
	}
	if a.sawContent {
		return res, errStreamUnterminated
	}
	return miniagent.Response{}, errors.New("stream ended without terminal response event")
}
