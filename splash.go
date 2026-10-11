package main

import (
	"image"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// ---------- 起動中の画面（フェーズ K4） ----------
//
// 設定を読んだらすぐに、フレームバッファと専用 VT を開いて「起動中」を出す。サービスと最初の画面を用意するあいだ
// （電源を入れた直後は、ほかのサービスと CPU を取り合うので数秒かかる）、ly のログイン画面のままにしない。
// 開いたフレームバッファと VT は、StartDisplay がそのまま引き継ぐ（開き直さないので、画面がちらつかない）。

// Splash は、起動中の画面を出しているフレームバッファと VT。
type Splash struct {
	fb   *Framebuffer
	vt   *VT
	cv   *Canvas
	sig  chan os.Signal
	stop chan struct{}
	wg   sync.WaitGroup
}

// splashText は、起動中の画面の文字。
const splashText = "起動中…"

// ShowSplash は、起動中の画面を出す。出せなければ nil（StartDisplay がいつもどおり開く）。
func ShowSplash(dc *DisplayConfig, rot int) *Splash {
	fb, err := OpenFramebuffer(dc.Device)
	if err != nil {
		return nil // StartDisplay が同じエラーをログに出す
	}
	sp := &Splash{fb: fb, sig: make(chan os.Signal, 4), stop: make(chan struct{})}
	signal.Notify(sp.sig, syscall.SIGUSR1, syscall.SIGUSR2)
	vt, err := OpenVT(dc.VT, syscall.SIGUSR1, syscall.SIGUSR2)
	if err != nil {
		signal.Stop(sp.sig)
		fb.Close()
		return nil
	}
	sp.vt = vt
	sp.cv = newCanvas(fb, rot)
	drawSplash(sp.cv, daemonVersion())
	if err := vt.Activate(); err != nil {
		log.Printf("splash: %v", err)
	}
	if vt.IsActive() {
		fb.Unblank()
		fb.Blit(sp.cv, image.Rect(0, 0, sp.cv.pw, sp.cv.ph))
	}
	// 引き継ぐまでに VT を切り替えられたら（ly が自分の VT に移るなど）、許可して、戻ってきたら描き直す
	sp.wg.Add(1)
	go func() {
		defer sp.wg.Done()
		for {
			select {
			case <-sp.stop:
				return
			case s := <-sp.sig:
				if s == syscall.SIGUSR1 {
					vt.Release()
				} else {
					vt.AckAcquire()
					fb.Blit(sp.cv, image.Rect(0, 0, sp.cv.pw, sp.cv.ph))
				}
			}
		}
	}()
	return sp
}

// handoff は、起動中の画面の後始末をして、フレームバッファと VT を返す。
// 呼ぶ前に、引き継ぐ側が SIGUSR1 と SIGUSR2 を受けられるようにしておく（そのあいだのシグナルを落とさない）。
func (sp *Splash) handoff() (*Framebuffer, *VT) {
	close(sp.stop)
	sp.wg.Wait()
	signal.Stop(sp.sig)
	// 受けたまま処理していないシグナルがあれば、ここで応える
	for {
		select {
		case s := <-sp.sig:
			if s == syscall.SIGUSR1 {
				sp.vt.Release()
			} else {
				sp.vt.AckAcquire()
			}
		default:
			return sp.fb, sp.vt
		}
	}
}

// Close は、引き継がずに終わるとき（画面を使わないと分かったとき）に、元の VT に戻す。
func (sp *Splash) Close() {
	if sp == nil {
		return
	}
	fb, vt := sp.handoff()
	vt.Close()
	fb.Close()
}

// drawSplash は、起動中の画面を描く。
func drawSplash(cv *Canvas, version string) {
	full := image.Rect(0, 0, cv.W, cv.H)
	cv.fill(full, colBG)
	const scale = 4
	w := font.textWidth(splashText) * scale
	cv.text((cv.W-w)/2, cv.H/2-12*scale/2-8, splashText, scale, colText, full)
	v := "brain-deck " + version
	cv.text((cv.W-font.textWidth(v)*2)/2, cv.H/2+12*scale/2+8, v, 2, colSub, full)
}
