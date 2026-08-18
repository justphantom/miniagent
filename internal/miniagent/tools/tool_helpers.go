// tool_helpers.go: helpers dedicated to tool construction/execution (used only by tool_*.go). Originally in the
// core tools.go, moved here to fix the physical misplacement of "core containing tool-specific helpers", paving
// the way for tool sub-packaging (library-ization 5.0.0). Logically belongs to the tool side, not the core loop;
// co-located in the same package only for historical physical layout, with no logical coupling (uses only public
// types + this group of helpers).

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// denyResult builds a pre-execution rejection: no command ran, so ExitCode must be ExitCodeNotSet —
// the zero value 0 would be misread by the event layer as success (P3-4).
func denyResult(format string, a ...any) miniagent.ToolResult {
	return miniagent.ToolResult{IsError: true, ExitCode: miniagent.ExitCodeNotSet, Output: fmt.Sprintf(format, a...)}
}

// resolveToolPath resolves a tool path: returns p unchanged when workspaceRoot is empty or p is already absolute;
// otherwise join(workspaceRoot, p) (join includes Clean, but ../ escaping upwards may resolve outside workdir).
// free mode has **no path boundary constraint**: both ../ and absolute paths can escape workdir; isolation is
// guaranteed by the caller (container/low-privilege user) (README §Execution Isolation). openNoFollow only rejects
// the final symlink component and does not constitute a boundary; the file size cap is unrelated to the boundary.
func resolveToolPath(workspaceRoot, p string) string {
	if workspaceRoot == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspaceRoot, p)
}

// decodeStrict unmarshals a tool-args JSON object rejecting unknown fields: a field-name typo
// (`{"subcommand":"add","command":"x"}`) used to fall through to EMPTY args, silently turning
// `git add` into a whole-repo stage. The error names the offending key so the LLM self-corrects.
// Trailing data after the object is likewise rejected: providers emit duplicated/concatenated
// payloads under retry/fragmentation, and silently keeping only the first object would make the
// LLM believe both invocations executed.
func decodeStrict(args string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing data after JSON object")
	}
	return nil
}

// sortedNames 与 map 同源生成逗号列表，避免手写串与 map 漂移（描述/错误共用的单一事实源）。
func sortedNames(m map[string]bool) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// maxFileResultInHistory is the character cap for results of code-content tools like read/edit entering history:
// code truncation means losing accuracy, so a higher quota than the default policy.MaxToolResultInHistory is given
// (still constrained by read's own maxReadFileChars output cap). miniagent.Tool.ResultLimit takes this value.
const maxFileResultInHistory = 8000

// object builds a JSON Schema object description. When required is empty the key is omitted: the JSON Schema
// spec states that omitting required is equivalent to an empty array, which all compliant backends accept;
// writing a nil slice into the map would serialize as "required":null, triggering a 400 from strict backends
// (e.g. OpenAI).
func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
