package miniagent

import (
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/text"
)

// chunkBuf 是流式累积器单个原始片段，text 与字节大小分离记录，
// 避免滑窗剔除时重复 len([]byte)（参照 opencode shell.ts 的 {text,size}）。私有，仅 output_accum.go 内用。
type chunkBuf struct {
	text string
	size int
}

// outputAccum 命令运行中按字节滑窗累积 stdout+stderr：内存只保最近 keep 字节（尾部），
// 超窗从最旧 chunk 剔除并置 cut（丢中段、保尾部——正好保住 shell 错误/退出码）；headSpillBytes>0 时
// 累计超阈值把头部追加落盘（O_APPEND，phase-2，默认关）。私有，runShellLimited 内构造。
type outputAccum struct {
	keep           int        // 滑窗字节上限（保尾部）；<=0 不限
	headSpillBytes int        // 落盘阈值；<=0 关闭（phase-1 默认）
	spillDir       string     // 落盘目录
	spillPrefix    string     // 落盘文件名前缀
	chunks         []chunkBuf // 从旧到新
	used           int        // chunks 总字节
	total          int        // 累计读入字节（含已剔除中段）
	cut            bool       // 是否丢弃过中段
	file           string     // 落盘文件路径（开落盘时）
	sink           *os.File   // 落盘句柄
}

// newOutputAccum 构造累积器。keep<=0 不限；headSpillBytes<=0 关闭落盘（phase-1 默认）。
// headSpillBytes 须 <= keep（否则滑窗在落盘阈值触发前就剔除头部，头部彻底丢失，与「头部落盘」承诺相悖）；
// keep<=0（不限窗，无剔除）时 headSpillBytes 任意正值都安全。违例自动夹紧到 keep（当前唯一调用点传 0）。
func newOutputAccum(keep, headSpillBytes int, spillDir, spillPrefix string) *outputAccum {
	if keep > 0 && headSpillBytes > keep {
		headSpillBytes = keep
	}
	return &outputAccum{keep: keep, headSpillBytes: headSpillBytes, spillDir: spillDir, spillPrefix: spillPrefix}
}

// write 推入一个 chunk：累加 total、append 到 chunks、超 headSpillBytes 调 spill（首次 dump 全部头部、
// 之后追加）、再 while used>keep && len>1 从 chunks[0] 剔除并 cut=true。
// 单 chunk 超 keep 且 len==1 不剔除（防空滑窗）。落盘失败 best-effort 上抛（调用方不中断捕获）。
func (a *outputAccum) write(chunk string) error {
	if len(chunk) == 0 {
		return nil
	}
	a.chunks = append(a.chunks, chunkBuf{text: chunk, size: len(chunk)})
	a.used += len(chunk)
	a.total += len(chunk)
	// 落盘：累计超阈值时首次创建 sink 并 dump 当前全部头部，之后每 chunk 追加。phase-1 headSpillBytes<=0 关闭。
	if a.headSpillBytes > 0 {
		if a.sink == nil && a.total >= a.headSpillBytes {
			if err := a.createSink(); err != nil {
				return err
			}
			for _, c := range a.chunks {
				if _, err := a.sink.WriteString(c.text); err != nil {
					return err
				}
			}
			a.file = a.sink.Name() // 首次 dump 全部成功后才置 file，finalize 据此决定 banner 是否标「全文：<file>」
		} else if a.sink != nil {
			if _, err := a.sink.WriteString(chunk); err != nil {
				return err
			}
		}
	}
	// 滑窗：超 keep 从最旧剔除（丢中段保尾）。单 chunk 超 keep 且 len==1 不剔除（防空滑窗）。
	for a.keep > 0 && a.used > a.keep && len(a.chunks) > 1 {
		old := a.chunks[0]
		a.chunks = a.chunks[1:]
		a.used -= old.size
		a.cut = true
	}
	return nil
}

// createSink 首次落盘时创建临时文件（os.CreateTemp 默认 0600），置 sink 与 cut。
// 不在此置 a.file——由 write 在首次 dump 全部 chunk 成功后置，否则 dump 中途失败时 finalize 仍标「全文：<file>」指向残文件。
func (a *outputAccum) createSink() error {
	f, err := os.CreateTemp(a.spillDir, a.spillPrefix+"*.log")
	if err != nil {
		return err
	}
	a.sink = f
	a.cut = true
	return nil
}

// closeSink 幂等关闭落盘句柄，finalize 前调用确保落盘。
func (a *outputAccum) closeSink() error {
	if a.sink == nil {
		return nil
	}
	err := a.sink.Close()
	a.sink = nil
	return err
}

// finalize 返回最终 Output：cut 时前置「…[输出超限，仅保留尾部[,全文：<file>]]\n」，
// 再 chunks join 经 text.TruncateTail(maxChars) 兜底。替代旧 truncate(out.String(),shellOutputChars(),"…")。
func (a *outputAccum) finalize(maxChars int) string {
	body := strings.Join(chunkTexts(a.chunks), "")
	body = text.TruncateTail(body, maxChars, "…[输出已截断]")
	if !a.cut {
		return body
	}
	banner := "…[输出超限，仅保留尾部"
	if a.file != "" {
		banner += "，全文：" + a.file
	}
	banner += "]\n"
	return banner + body
}

// chunkTexts 抽取 chunks 的 text 切片（仅供 finalize join 用）。
func chunkTexts(chunks []chunkBuf) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.text
	}
	return out
}
