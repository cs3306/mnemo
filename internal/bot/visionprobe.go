package bot

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"companion/internal/llm"
)

// A vision backend that does not actually accept images (for example deepseek-v4-pro) may not return an
// error: the image part is ignored and the model invents a plausible "transcription" from the prompt alone.
// The probe renders a random 4-digit code into a small PNG and asks the vision model to read it. A model that
// cannot see the image cannot guess the code, so a wrong answer proves the backend is not reading images.
const (
	visionUnknown int32 = iota // not probed yet, or the probe could not get an answer (network, auth, ...)
	visionOK
	visionBad // the model answered, but not with the digits in the image
)

var errVisionBad = errors.New("读图模型自检未通过：它没能读出测试图里的数字，转写内容会是编造的。" +
	"请把 VISION_MODEL 换成支持图片输入的模型；改好后发 /visiontest 重新检测（偶发读错也会触发，可以多测几次）")

const probePrompt = "图片里是一串数字。请只输出这串数字，不要输出别的。"

// 5x7 bitmap digits.
var digitGlyphs = map[rune][7]string{
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	'6': {"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
}

// probeImage renders the digits of code (black on white) as a PNG.
func probeImage(code string) []byte {
	const cell = 16
	digits := []rune(code)
	w, h := (len(digits)*6+3)*cell, (7+4)*cell
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for i, d := range digits {
		g, ok := digitGlyphs[d]
		if !ok {
			continue
		}
		for y, row := range g {
			for x, c := range row {
				if c != '1' {
					continue
				}
				x0, y0 := (2+i*6+x)*cell, (2+y)*cell
				for yy := y0; yy < y0+cell; yy++ {
					for xx := x0; xx < x0+cell; xx++ {
						img.SetGray(xx, yy, color.Gray{Y: 0})
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func randomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return "4821"
	}
	return fmt.Sprintf("%d", 1000+n.Int64())
}

// probeVision checks that the vision backend really reads images and records the result: a wrong answer blocks
// image reading (visionBad) until a later probe passes; request failures leave the state unchanged.
func (a *App) probeVision(ctx context.Context) error {
	if a.vision == nil {
		return errVisionOff
	}
	code := randomCode()
	if a.probeCode != nil {
		code = a.probeCode()
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := a.vision.Complete(ctx, []llm.Message{llm.Image(probePrompt, probeImage(code), "image/png")}, 0)
	if err != nil {
		return fmt.Errorf("读图自检请求失败：%w", err)
	}
	var digits strings.Builder
	for _, r := range out {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	if strings.Contains(digits.String(), code) {
		a.visionState.Store(visionOK)
		return nil
	}
	a.visionState.Store(visionBad)
	return fmt.Errorf("%w（图里是 %s，模型回答：%s）", errVisionBad, code, truncate(strings.TrimSpace(out), 60))
}

// startupProbe runs the probe once at startup and logs the outcome.
func (a *App) startupProbe(ctx context.Context) {
	err := a.probeVision(ctx)
	switch {
	case err == nil:
		slog.Info("vision self-test passed", "model", a.cfg.Vision.Model)
	case errors.Is(err, errVisionBad):
		slog.Error("vision self-test FAILED: the vision model does not read images; image reading is blocked", "model", a.cfg.Vision.Model, "detail", err.Error())
	default:
		slog.Warn("vision self-test could not run", "err", err)
	}
}
