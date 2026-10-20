// Package ui renders the fixed download interface.
//
// The renderer is line based: a TTY gets an in-place redrawn status block with
// a progress bar and an optional QR code, a non-TTY gets plain progress lines.
// It never reads input and never touches the wallet; the caller owns all
// decisions.
package ui

import (
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
)

// Stage is one download phase.
type Stage string

const (
	StageUnlock    Stage = "unlock"
	StageDiscovery Stage = "discovery"
	StageWaiting   Stage = "waiting"
	StageQuote     Stage = "quote"
	StageOpening   Stage = "opening"
	StageSeed      Stage = "seed"
	StageBlocks    Stage = "blocks"
	StageClosing   Stage = "closing"
	StageAssembled Stage = "assembled"
	StageDone      Stage = "done"
	StageFailed    Stage = "failed"
)

// State is the current interface projection.
type State struct {
	Stage      Stage
	Address    string
	SeedHash   string
	Seller     string
	FileSize   uint64
	Blocks     int
	BlocksDone int
	Paid       uint64
	Detail     string
	Path       string
	Error      string
	// Price is the scheduler price state (empty when not scheduled).
	PriceMode      string
	PriceEffective string
	PriceMin       string
	PriceMax       string
}

// Renderer draws one interface.
type Renderer struct {
	writer  io.Writer
	tty     bool
	qr      bool
	mu      sync.Mutex
	lastLen int
}

// New creates a renderer. tty enables in-place redraws; showQR adds a QR code
// of the wallet address on the unlock view (only meaningful on a TTY).
func New(writer io.Writer, tty, showQR bool) *Renderer {
	return &Renderer{writer: writer, tty: tty, qr: showQR}
}

// Render draws the state.
func (r *Renderer) Render(state State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	block := r.block(state)
	if r.tty {
		// Clear the previous block by rewriting lines.
		if r.lastLen > 0 {
			fmt.Fprintf(r.writer, "\x1b[%dA", r.lastLen)
			for index := 0; index < r.lastLen; index++ {
				fmt.Fprint(r.writer, "\x1b[2K")
				if index < r.lastLen-1 {
					fmt.Fprint(r.writer, "\x1b[1B")
				}
			}
			fmt.Fprintf(r.writer, "\x1b[%dA", r.lastLen-1)
		}
		fmt.Fprintln(r.writer, block)
		r.lastLen = strings.Count(block, "\n") + 1
		return
	}
	fmt.Fprintln(r.writer, block)
	r.lastLen = 0
}

func (r *Renderer) block(state State) string {
	lines := make([]string, 0, 12)
	lines = append(lines, "km 下载")
	if state.Address != "" {
		lines = append(lines, "钱包地址: "+state.Address)
	}
	if r.qr && r.tty && state.Address != "" {
		if art, err := qrArt(state.Address); err == nil {
			lines = append(lines, art)
		}
	}
	if state.SeedHash != "" {
		lines = append(lines, "Seed: "+state.SeedHash)
	}
	if state.Seller != "" {
		lines = append(lines, "卖方: "+state.Seller)
	}
	switch state.Stage {
	case StageUnlock:
		lines = append(lines, "状态: 钱包已解锁")
	case StageDiscovery:
		lines = append(lines, "状态: 已发布需求，等待供应商投递")
	case StageWaiting:
		lines = append(lines, "状态: 等待卖方连接…")
	case StageQuote:
		lines = append(lines, "状态: 收到报价，准备开池 "+state.Detail)
	case StageOpening:
		lines = append(lines, "状态: 开池中（Kind 2/3/4）")
	case StageSeed:
		lines = append(lines, "状态: 验证 Seed")
	case StageBlocks:
		lines = append(lines, fmt.Sprintf("状态: 下载块 %d/%d", state.BlocksDone, state.Blocks))
		lines = append(lines, bar(state.BlocksDone, state.Blocks))
	case StageClosing:
		lines = append(lines, "状态: 关池结算（等待卖方广播）")
	case StageAssembled:
		lines = append(lines, "状态: 校验并写入文件")
	case StageDone:
		lines = append(lines, fmt.Sprintf("状态: 完成 %s（%d 字节）", state.Path, state.FileSize))
		if state.Paid > 0 {
			lines = append(lines, fmt.Sprintf("已付款: %d sats", state.Paid))
		}
	case StageFailed:
		lines = append(lines, "状态: 失败")
		if state.Error != "" {
			lines = append(lines, "原因: "+state.Error)
		}
	}
	if state.PriceEffective != "" && state.Stage != StageDone && state.Stage != StageFailed {
		lines = append(lines, "价格: "+state.PriceEffective+" sats/块 "+priceBar(state.PriceMin, state.PriceEffective, state.PriceMax)+" "+state.PriceMode+"  (↑/↓ 调整, a 自动)")
	}
	if state.Detail != "" && state.Stage != StageQuote {
		lines = append(lines, state.Detail)
	}
	return strings.Join(lines, "\n")
}

func bar(done, total int) string {
	const width = 28
	if total <= 0 {
		return "[" + strings.Repeat(" ", width) + "]"
	}
	if done > total {
		done = total
	}
	filled := done * width / total
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

// qrArt renders a compact block QR (two spaces per module for aspect ratio).
func qrArt(value string) (string, error) {
	code, err := qrcode.New(value, qrcode.Low)
	if err != nil {
		return "", err
	}
	code.DisableBorder = true
	rows := code.Bitmap()
	var builder strings.Builder
	for index, row := range rows {
		if index > 0 {
			builder.WriteByte('\n')
		}
		for _, dark := range row {
			if dark {
				builder.WriteString("██")
			} else {
				builder.WriteString("  ")
			}
		}
	}
	return builder.String(), nil
}

// priceBar renders the effective price between min and max.
func priceBar(minimum, effective, maximum string) string {
	const width = 24
	minValue, okMin := new(big.Int).SetString(minimum, 10)
	effValue, okEff := new(big.Int).SetString(effective, 10)
	maxValue, okMax := new(big.Int).SetString(maximum, 10)
	if !okMin || !okEff || !okMax || maxValue.Cmp(minValue) <= 0 {
		return "[" + effective + "]"
	}
	position := new(big.Int).Sub(effValue, minValue)
	widthBig := new(big.Int).Sub(maxValue, minValue)
	position.Mul(position, big.NewInt(width-1))
	position.Div(position, widthBig)
	index := int(position.Int64())
	if index < 0 {
		index = 0
	}
	if index > width-1 {
		index = width - 1
	}
	cells := make([]byte, width)
	for i := range cells {
		cells[i] = '-'
	}
	cells[index] = '*'
	return "[" + string(cells) + "]"
}
