package tools

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// utf8FullRune 判定对象是 chunk[:cut] 这类前缀：末字节是否落在 rune 边界。
// 表驱动覆盖 ASCII 尾、完整多字节尾、rune 中间的续字节尾、孤立首字节尾、超过 4 个续字节
// （非 UTF-8 流）五种形态；lone-lead 尾曾误判为完整 rune（run_limited.go #3）。
func TestUtf8FullRune(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"empty slice", nil, false},
		{"ascii tail", []byte("abc"), true},
		{"complete 2-byte tail", []byte("a\xc3\xa9"), true},
		{"complete 3-byte tail", []byte("a\xe4\xbd\xa0"), true},
		{"complete 4-byte tail", []byte("a\xf0\x9f\x98\x80"), true},
		{"mid-rune continuation tail (2-byte cut)", []byte("a\xc3"), false},
		{"mid-rune continuation tail (3-byte cut)", []byte("a\xe4\xbd"), false},
		{"mid-rune continuation tail (4-byte cut)", []byte("a\xf0\x9f\x98"), false},
		{"lone lead tail (2-byte lead)", []byte("a\xc3\xa9\xc3"), false},
		{"lone lead tail (3-byte lead)", []byte("a\xe4\xbd\xa0\xe4"), false},
		{"lone lead tail (4-byte lead)", []byte("a\xf0\x9f\x98\x80\xf0"), false},
		{"lone lead tail at slice start", []byte("\xe4"), false},
		{">4 continuation bytes (non-UTF-8)", []byte("a\x80\x80\x80\x80\x80"), false},
	}
	for _, c := range cases {
		if got := utf8FullRune(c.b); got != c.want {
			t.Errorf("%s: utf8FullRune(% x) = %v, want %v", c.name, c.b, got, c.want)
		}
	}
}

// 回扫上界 i >= len(b)-4：合法 rune 至多 4 字节，第 5 个字节起再往前必是另一个 rune，
// 不回扫（本例 len(b)-5 处的首字节与续字节计数不匹配，仍判 false，与逐字节回退一致）。
func TestUtf8FullRune_ScanBoundFourBytes(t *testing.T) {
	// "a" + 3-byte rune + 2 stray continuation bytes: scan window covers the strays only.
	if utf8FullRune([]byte("a\xe4\xbd\xa0\x80\x80")) {
		t.Error("two trailing continuation bytes after a complete rune must not be a boundary")
	}
}

// drainPipe 经 pending 缓冲缝合跨 Read 边界的 rune：每次 Write 一个 3 字节字符、Read 尺寸
// 与 3 互质（奇数次对齐错开），写出的每个 chunk 都必须自身是有效 UTF-8（#4/#3 回归）。
func TestDrainPipe_ChunkBoundaryRuneAlignment(t *testing.T) {
	pr, pw := io.Pipe()
	accum := newOutputAccum(100000, 0, "", "drain_")
	done := make(chan struct{})
	go func() { drainPipe(pr, accum); close(done) }()
	go func() {
		cjk := []byte("你") // E4 BD A0, 3 字节
		for range 60000 {
			_, _ = pw.Write(cjk)
		}
		_ = pw.Close()
	}()
	<-done
	_ = accum.closeSink()
	if len(accum.chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(accum.chunks))
	}
	for i, c := range accum.chunks {
		if !utf8.ValidString(c.text) {
			t.Fatalf("chunk %d is not valid UTF-8: head=% x tail=% x", i, c.text[:3], c.text[len(c.text)-3:])
		}
	}
	body := strings.Join(chunkTexts(accum.chunks), "")
	if !utf8.ValidString(body) {
		t.Errorf("joined body not valid UTF-8: head=% x", body[:8])
	}
}

// 非 UTF-8 流（连续 0x80 字节、无首字节）：pending 被 cap 在 4 字节，滑动窗口照常逐出
// （banner 出现）、调用及时返回，而不是整段滞留 pending（#2 回归，旧代码窗口永不逐出）。
func TestRunLimitedOutput_NonUTF8WindowStillEvicts(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", `head -c 2000000 /dev/zero | tr '\000' '\200'`)
	done := make(chan struct{})
	var body string
	var err error
	go func() {
		body, err = runLimitedOutput(ctx, cmd, 100000)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runLimitedOutput did not return for a non-UTF-8 stream (pending grew unbounded)")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "only tail kept") {
		t.Errorf("sliding window never evicted for non-UTF-8 output: len=%d", len(body))
	}
	if len(body) > 400000 { // banner + tail + TruncateTail(100000 runes) 的余量
		t.Errorf("output far exceeds the window/cap: len=%d", len(body))
	}
}

// 合法 UTF-8 输出与旧行为按字节等价：内容原样保留、无 banner（#2 的“valid-UTF-8 行为不变”约束）。
func TestRunLimitedOutput_ValidUTF8Unchanged(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", `awk 'BEGIN{for(i=0;i<50;i++)printf "你ok\n"}'`)
	body, err := runLimitedOutput(ctx, cmd, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(body, "only tail kept") {
		t.Errorf("under-limit output should carry no banner: %q", firstN(body, 40))
	}
	if !utf8.ValidString(body) {
		t.Errorf("valid UTF-8 input must round-trip byte-identically: % x", body[:6])
	}
	if !strings.Contains(body, "你ok") || !strings.HasSuffix(body, "你ok\n") {
		t.Errorf("content altered: head=%q tail=%q", firstN(body, 10), body[len(body)-8:])
	}
}

// 窗口按字节、cap 按字符（#29）：200000 字节 / 66666 个 CJK 字符在 100000 字符 cap 之内，
// 不得再被 maxOutputChars 原值（字节窗口）过度逐出——旧实现只剩 ~12000 字符 + banner。
func TestRunLimitedOutput_MultibyteNotOverEvicted(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available")
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", `awk 'BEGIN{for(i=0;i<66666;i++)printf "你"}'`)
	body, err := runLimitedOutput(ctx, cmd, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(body, "only tail kept") {
		t.Errorf("under-char-cap CJK output should not be evicted: %q runes=%d", firstN(body, 30), len([]rune(body)))
	}
	if got := len([]rune(body)); got != 66666 {
		t.Errorf("runes = %d, want 66666 (8x byte window must keep the in-cap content)", got)
	}
}

// setsid 孤孙子进程持管道时 cmd.Wait 挂死：waitOutputTimeout 在基线 +2s 后放弃等待，
// 调用方有界返回并报告超时（#1 回归；泄漏的孤儿子进程是已知残留，见 platform.go P3-6）。
func TestShell_SetsidOrphanBoundedWait(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	if testing.Short() {
		t.Skip("requires the timeout+grace window to elapse")
	}
	s := ShellTool(t.TempDir(), 500*time.Millisecond, 0, 0)
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"setsid sleep 10 & echo started"}`)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("wait not bounded: elapsed=%v (orphan still holds the pipe)", elapsed)
	}
	if !res.IsError || res.ExitCode != -1 {
		t.Errorf("orphaned-pipe timeout should be IsError + ExitCodeNotSet: isErr=%v exit=%d", res.IsError, res.ExitCode)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("timeout must be observable: %q", res.Output)
	}
}

// 后台 & 进程持有管道到超时、组长早已干净退出（err==nil）：超时仍必须可观测（#28 回归，
// 旧实现返回 IsError=false + 干净成功，1ms 的命令静默吃掉整个超时）。
func TestShell_BackgroundJobTimeoutObservable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the timeout to elapse")
	}
	s := ShellTool(t.TempDir(), 1*time.Second, 0, 0)
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"sleep 30 & echo started"}`)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	if !res.IsError {
		t.Error("background-holder timeout should be IsError=true")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (ExitCodeNotSet)", res.ExitCode)
	}
	if !strings.Contains(res.Output, "timed out") || !strings.Contains(res.Output, "background output holders terminated") {
		t.Errorf("timeout hint missing: %q", res.Output)
	}
	if !strings.Contains(res.Output, "started") {
		t.Errorf("partial output lost: %q", res.Output)
	}
}

// 未知字段必须报错（#19）：旧实现用宽松的 json.Unmarshal，{"command":..,"workdir":..} 里的
// 拼错字段被静默丢弃，LLM 以为参数生效。与 git/go/npm/lint 的 decodeStrict 策略对齐。
func TestShell_UnknownFieldRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), 0, 0, 0)
	res := s.Call(context.Background(), `{"command":"echo hi","workdir":"/tmp"}`)
	if !res.IsError {
		t.Fatal("unknown field should be rejected")
	}
	if !strings.Contains(res.Output, "unknown field") {
		t.Errorf("error should name the offending key for self-correction: %q", res.Output)
	}
}

// shell 滑动窗口逐出后的尾部必须是有效 UTF-8（#4 回归：shell 曾无 pending 缓冲，
// 尾部以半个 rune 开头，首个可见字符成 U+FFFD）。streamWindow 压到与 cap 相等以强制逐出。
func TestShell_EvictedTailKeepsRuneBoundary(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available")
	}
	s := ShellTool(t.TempDir(), 30*time.Second, 100000, 100000)
	res := s.Call(context.Background(), `{"command":"awk 'BEGIN{for(i=0;i<60000;i++)printf \"你\"}'"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	body := strings.TrimPrefix(res.Output, "…[output over limit, only tail kept]\n")
	if !utf8.ValidString(body) {
		t.Errorf("evicted tail starts mid-rune: head=% x", body[:6])
	}
}

// 纯二进制流不再输出成片 U+FFFD：无效字节按十六进制转义，输出可读且不超过字符上限的量级。
func TestRunLimitedOutput_NonUTF8HexEscaped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", `head -c 400000 /dev/zero | tr '\000' '\201\202'`)
	body, err := runLimitedOutput(ctx, cmd, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	invalid := strings.Count(body, "�")
	if invalid > 0 {
		t.Errorf("non-UTF-8 output should be hex-escaped, found %d U+FFFD in %d chars", invalid, len([]rune(body)))
	}
	if !strings.Contains(body, "80 ") && !strings.Contains(body, "81 ") && !strings.Contains(body, "82 ") {
		t.Errorf("expected hex escapes in output, head: %q", firstN(body, 60))
	}
	if r := len([]rune(body)); r > 350000 { // 十六进制 3 字符/字节 + banner + TruncateTail 余量
		t.Errorf("escaped output exceeds expected bound: %d runes", r)
	}
}
