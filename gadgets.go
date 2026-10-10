package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

// ---------- タップで操作するウィジェットの状態（ストップウォッチ、ポモドーロ、カウンター、在席ランプ） ----------
//
// 状態は /var/lib/lefthand/widgets.json に、ウィジェットの種類と id ごとに置く。同じ id のセルは同じ状態を出す。
// テキストと同じく、保存は 1 つの goroutine がまとめて行い、タップの処理は SD カードへの書き込みを待たない。
//
// 動いているストップウォッチとポモドーロは、始めた時刻（壁時計）を覚える。
// - デーモンの再起動：同じ起動（boot_id）のあいだは、始めた時刻から数え続ける。
// - Brain の再起動：Brain には RTC がなく、起動直後の時刻は当てにならない。終了するときに書いた時刻（last_seen）
//   までを数えて止める（一時停止）。電池が切れたときなど、終了の処理をしなかったときは、最後に始めた時点までになる。
// - 時刻合わせ（set_time）でシステムの時刻が動いたら、始めた時刻も同じだけ動かす（経過時間を保つ）。

const (
	gadgetStoreName = "widgets"
	gadgetMaxIDs    = 64 // 種類ごとに覚える id の数。超えたら、いちばん前に変えたものを捨てる

	pomoAlert      = 15 * time.Second       // 作業と休憩が切り替わってから、セルを点滅させる時間
	pomoBlink      = 500 * time.Millisecond // 点滅の半周期
	counterMax     = 999999999
	defaultWork    = 25 * time.Minute
	defaultBreak   = 5 * time.Minute
	minPomoPhase   = time.Minute
	maxPomoPhase   = 4 * time.Hour
	pomoPhaseWork  = "work"
	pomoPhaseBreak = "break"
)

// StopwatchState は 1 つのストップウォッチ。
type StopwatchState struct {
	ElapsedMS int64      `json:"elapsed_ms"`           // 止めているあいだの経過時間（動いているときは、始める前までの分）
	StartedAt *time.Time `json:"started_at,omitempty"` // 動いているなら、始めた時刻
	UpdatedAt time.Time  `json:"updated_at"`
}

// Elapsed は now の時点の経過時間。
func (s StopwatchState) Elapsed(now time.Time) time.Duration {
	d := time.Duration(s.ElapsedMS) * time.Millisecond
	if s.StartedAt != nil && now.After(*s.StartedAt) {
		d += now.Sub(*s.StartedAt)
	}
	return d
}

// PomodoroState は 1 つのポモドーロ。動いているあいだは、作業を始めた（ことにした）時刻 Anchor から、
// 作業 WorkMS と休憩 BreakMS を繰り返したとして、今の段階を計算で求める（状態を書き換えずに描ける）。
type PomodoroState struct {
	Running bool       `json:"running"`
	Anchor  *time.Time `json:"anchor,omitempty"`  // running：今の周期より前の周期も含めた、最初の作業の始まり
	WorkMS  int64      `json:"work_ms,omitempty"` // running と一時停止：始めたときのセルの設定
	BreakMS int64      `json:"break_ms,omitempty"`
	Done    int        `json:"done"` // running：Anchor より前に終えた作業の数。止めているとき：終えた作業の数
	// 止めているとき（一時停止）の段階と残り。Phase が空なら、まだ始めていない（作業の全部が残っている）
	Phase       string     `json:"phase,omitempty"`
	RemainingMS int64      `json:"remaining_ms,omitempty"`
	AckAt       *time.Time `json:"ack_at,omitempty"` // 点滅をタップで止めた時刻
	UpdatedAt   time.Time  `json:"updated_at"`
}

// pomoView は、ポモドーロの now の時点の様子。
type pomoView struct {
	Started   bool // 一度でも始めた（リセットのあとは false）
	Running   bool
	Phase     string // work か break
	Remaining time.Duration
	Count     int       // 終えた作業の数
	Switched  time.Time // 最後に段階が切り替わった時刻（動いているときだけ。始めてから一度も切り替わっていなければゼロ）
	Alert     bool      // 切り替わってから pomoAlert のあいだ（タップで止めていない）
	BlinkOn   bool      // 点滅の、塗る側
	Next      time.Time // 次に表示が変わる時刻
}

// view は、ポモドーロの now の時点の様子を求める。work は、まだ始めていないときに出す作業の時間（セルの設定）。
func (p PomodoroState) view(now time.Time, work time.Duration) pomoView {
	if !p.Running || p.Anchor == nil || p.WorkMS <= 0 || p.BreakMS <= 0 {
		v := pomoView{Started: p.Phase != "", Phase: pomoPhaseWork, Remaining: work, Count: p.Done}
		if v.Started {
			v.Phase, v.Remaining = p.Phase, time.Duration(p.RemainingMS)*time.Millisecond
		}
		return v
	}
	w, b := time.Duration(p.WorkMS)*time.Millisecond, time.Duration(p.BreakMS)*time.Millisecond
	el := max(now.Sub(*p.Anchor), 0)
	k := el / (w + b)
	in := el % (w + b)
	start := p.Anchor.Add(k * (w + b)) // 今の周期の始まり
	v := pomoView{Started: true, Running: true, Count: p.Done + int(k)}
	if in < w {
		v.Phase, v.Remaining = pomoPhaseWork, w-in
		if k > 0 {
			v.Switched = start
		}
	} else {
		v.Phase, v.Remaining = pomoPhaseBreak, w+b-in
		v.Count++
		v.Switched = start.Add(w)
	}
	// 表示は秒単位（残りの秒を切り上げる）。次の秒の切り替わりで描き直す
	frac := v.Remaining % time.Second
	if frac == 0 {
		frac = time.Second
	}
	v.Next = now.Add(frac)
	if !v.Switched.IsZero() && now.Sub(v.Switched) < pomoAlert && (p.AckAt == nil || p.AckAt.Before(v.Switched)) {
		v.Alert = true
		since := now.Sub(v.Switched)
		v.BlinkOn = (since/pomoBlink)%2 == 0
		if t := v.Switched.Add((since/pomoBlink + 1) * pomoBlink); t.Before(v.Next) {
			v.Next = t
		}
	}
	return v
}

// PresenceState は 1 つの在席ランプ。状態の名前を覚える（色と並びはセルの設定）。
type PresenceState struct {
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
	Source    string    `json:"source,omitempty"`
}

// CounterState は 1 つのカウンター。
type CounterState struct {
	Value     int64     `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// gadgetFile は widgets.json の形。描画には、この写しを渡す。
type gadgetFile struct {
	BootID    string                    `json:"boot_id,omitempty"`   // 書いたときの起動
	LastSeen  *time.Time                `json:"last_seen,omitempty"` // デーモンが終わった時刻（動いているタイマーを止める時刻）
	Stopwatch map[string]StopwatchState `json:"stopwatch,omitempty"`
	Pomodoro  map[string]PomodoroState  `json:"pomodoro,omitempty"`
	Counter   map[string]CounterState   `json:"counter,omitempty"`
	Presence  map[string]PresenceState  `json:"presence,omitempty"`
	// Presenter はプレゼン用タイマー。持ち時間はセルの設定なので、経過時間だけを覚える（ストップウォッチと同じ形）
	Presenter map[string]StopwatchState `json:"presenter,omitempty"`
}

func (f gadgetFile) clone() gadgetFile {
	g := f
	g.Stopwatch = cloneMap(f.Stopwatch)
	g.Pomodoro = cloneMap(f.Pomodoro)
	g.Counter = cloneMap(f.Counter)
	g.Presence = cloneMap(f.Presence)
	g.Presenter = cloneMap(f.Presenter)
	return g
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// GadgetService は、タップで操作するウィジェットの状態を持ち、保存する。
type GadgetService struct {
	mu       sync.Mutex
	f        gadgetFile
	store    *Store
	bootID   string
	onChange func()           // 状態が変わったとき（画面の描き直し）。待たずに返ること
	onAlert  func()           // ポモドーロの段階が切り替わったとき（画面を明るくする）。待たずに返ること
	onNotify func(gadgetFile) // 状態が変わったとき（設定 GUI への通知）。待たずに返ること
	onBusy   func(bool)       // プレゼン用タイマーが動き始めた・すべて止まったとき（自動減光を止める）。待たずに返ること
	busy     bool             // プレゼン用タイマーのどれかが動いている
	alert    *time.Timer
	dirty    chan struct{}
	saved    chan struct{} // テスト用
}

func NewGadgetService(store *Store) *GadgetService {
	g := &GadgetService{store: store, bootID: readBootID(), dirty: make(chan struct{}, 1)}
	g.f = gadgetFile{Stopwatch: map[string]StopwatchState{}, Pomodoro: map[string]PomodoroState{},
		Counter: map[string]CounterState{}, Presence: map[string]PresenceState{}, Presenter: map[string]StopwatchState{}}
	var f gadgetFile
	if err := store.Load(gadgetStoreName, &f); err == nil {
		g.restore(f)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("widgets: %v", err)
	}
	go g.saver()
	g.mu.Lock()
	g.scheduleAlertLocked(time.Now())
	g.mu.Unlock()
	return g
}

// restore は保存した状態を読み込む。Brain を再起動したあとなら、動いていたタイマーを止める。
func (g *GadgetService) restore(f gadgetFile) {
	for id, s := range f.Stopwatch {
		if validTextID(id) {
			g.f.Stopwatch[id] = s
		}
	}
	for id, s := range f.Pomodoro {
		if validTextID(id) {
			g.f.Pomodoro[id] = s
		}
	}
	for id, s := range f.Counter {
		if validTextID(id) {
			g.f.Counter[id] = s
		}
	}
	for id, s := range f.Presence {
		if validTextID(id) {
			g.f.Presence[id] = s
		}
	}
	for id, s := range f.Presenter {
		if validTextID(id) {
			g.f.Presenter[id] = s
		}
	}
	if f.BootID == g.bootID || f.BootID == "" {
		return // 同じ起動のあいだ（デーモンの再起動）：そのまま数え続ける
	}
	// Brain を再起動した。終了したときの時刻（なければ最後に始めた時点）で止める
	n := 0
	for _, m := range []map[string]StopwatchState{g.f.Stopwatch, g.f.Presenter} {
		for id, s := range m {
			if s.StartedAt == nil {
				continue
			}
			if f.LastSeen != nil {
				s.ElapsedMS = s.Elapsed(*f.LastSeen).Milliseconds()
			}
			s.StartedAt = nil
			m[id] = s
			n++
		}
	}
	for id, p := range g.f.Pomodoro {
		if !p.Running {
			continue
		}
		at := time.Time{}
		if f.LastSeen != nil {
			at = *f.LastSeen
		} else if p.Anchor != nil {
			at = *p.Anchor
		}
		g.f.Pomodoro[id] = p.pauseAt(at)
		n++
	}
	if n > 0 {
		log.Printf("widgets: the Brain restarted; stopped %d running stopwatch/pomodoro/presenter timers", n)
	}
}

// pauseAt は、t の時点で一時停止したポモドーロを返す。
func (p PomodoroState) pauseAt(t time.Time) PomodoroState {
	v := p.view(t, 0)
	return PomodoroState{WorkMS: p.WorkMS, BreakMS: p.BreakMS, Done: v.Count, Phase: v.Phase,
		RemainingMS: v.Remaining.Milliseconds(), UpdatedAt: t.UTC()}
}

// SetOnChange は、状態が変わったときに呼ぶ関数を設定する。
func (g *GadgetService) SetOnChange(f func()) {
	g.mu.Lock()
	g.onChange = f
	g.mu.Unlock()
}

// SetOnAlert は、ポモドーロの段階が切り替わったときに呼ぶ関数を設定する（画面を明るくする）。
func (g *GadgetService) SetOnAlert(f func()) {
	g.mu.Lock()
	g.onAlert = f
	g.mu.Unlock()
}

// SetOnNotify は、状態が変わったときに、状態の写しを渡して呼ぶ関数を設定する（設定 GUI への通知）。
func (g *GadgetService) SetOnNotify(f func(gadgetFile)) {
	g.mu.Lock()
	g.onNotify = f
	g.mu.Unlock()
}

// SetOnBusy は、プレゼン用タイマーが動き始めたとき（true）と、すべて止まったとき（false）に呼ぶ関数を設定する。
// 設定したときに、今の様子で一度呼ぶ。
func (g *GadgetService) SetOnBusy(f func(bool)) {
	g.mu.Lock()
	g.onBusy = f
	g.busy = g.presenterRunningLocked()
	busy := g.busy
	g.mu.Unlock()
	if f != nil {
		f(busy)
	}
}

func (g *GadgetService) presenterRunningLocked() bool {
	for _, s := range g.f.Presenter {
		if s.StartedAt != nil {
			return true
		}
	}
	return false
}

// Snapshot は、今の状態の写し。描画の goroutine が描き直すたびに使う。
func (g *GadgetService) Snapshot() gadgetFile {
	if g == nil {
		return gadgetFile{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.f.clone()
}

// update は、ロックを持って f を呼び、状態が変わったら保存と描き直しを頼む。
func (g *GadgetService) update(f func(now time.Time) bool) {
	if g == nil {
		return
	}
	now := time.Now()
	g.mu.Lock()
	changed := f(now)
	if changed {
		g.evictLocked()
		g.scheduleAlertLocked(now)
	}
	cb, nf := g.onChange, g.onNotify
	var snap gadgetFile
	if changed && nf != nil {
		snap = g.f.clone()
	}
	busyCb, busy := g.onBusy, g.presenterRunningLocked()
	busyChanged := changed && busy != g.busy
	if busyChanged {
		g.busy = busy
	}
	g.mu.Unlock()
	if !changed {
		return
	}
	if busyChanged && busyCb != nil {
		busyCb(busy)
	}
	select {
	case g.dirty <- struct{}{}:
	default:
	}
	if cb != nil {
		cb()
	}
	if nf != nil {
		nf(snap)
	}
}

func evictOldest[V any](m map[string]V, at func(V) time.Time, kind string) {
	for len(m) > gadgetMaxIDs {
		oldest := ""
		for id, v := range m {
			if oldest == "" || at(v).Before(at(m[oldest])) {
				oldest = id
			}
		}
		log.Printf("widgets: more than %d %s ids, dropping %q", gadgetMaxIDs, kind, oldest)
		delete(m, oldest)
	}
}

func (g *GadgetService) evictLocked() {
	evictOldest(g.f.Stopwatch, func(s StopwatchState) time.Time { return s.UpdatedAt }, "stopwatch")
	evictOldest(g.f.Pomodoro, func(s PomodoroState) time.Time { return s.UpdatedAt }, "pomodoro")
	evictOldest(g.f.Counter, func(s CounterState) time.Time { return s.UpdatedAt }, "counter")
	evictOldest(g.f.Presence, func(s PresenceState) time.Time { return s.UpdatedAt }, "presence")
	evictOldest(g.f.Presenter, func(s StopwatchState) time.Time { return s.UpdatedAt }, "presenter")
}

// ---------- ストップウォッチ ----------

// StopwatchToggle は、止まっていれば始め、動いていれば止める。
func (g *GadgetService) StopwatchToggle(id string) {
	g.update(func(now time.Time) bool {
		s := g.f.Stopwatch[id]
		if s.StartedAt != nil {
			s.ElapsedMS = s.Elapsed(now).Milliseconds()
			s.StartedAt = nil
		} else {
			t := now.UTC()
			s.StartedAt = &t
		}
		s.UpdatedAt = now.UTC()
		g.f.Stopwatch[id] = s
		return true
	})
}

// StopwatchReset は 0 に戻して止める。
func (g *GadgetService) StopwatchReset(id string) {
	g.update(func(now time.Time) bool {
		g.f.Stopwatch[id] = StopwatchState{UpdatedAt: now.UTC()}
		return true
	})
}

// ---------- プレゼン用タイマー ----------

// PresenterToggle は、止まっていれば始め（続け）、動いていれば一時停止する。
func (g *GadgetService) PresenterToggle(id string) {
	g.update(func(now time.Time) bool {
		s := g.f.Presenter[id]
		if s.StartedAt != nil {
			s.ElapsedMS = s.Elapsed(now).Milliseconds()
			s.StartedAt = nil
		} else {
			t := now.UTC()
			s.StartedAt = &t
		}
		s.UpdatedAt = now.UTC()
		g.f.Presenter[id] = s
		return true
	})
}

// PresenterReset は、0 に戻して止める。
func (g *GadgetService) PresenterReset(id string) {
	g.update(func(now time.Time) bool {
		g.f.Presenter[id] = StopwatchState{UpdatedAt: now.UTC()}
		return true
	})
}

// ---------- ポモドーロ ----------

// PomodoroTap は、点滅していれば点滅を止め、止まっていれば始め（続け）、動いていれば一時停止する。
// work と brk は、タップしたセルの設定。始めるときにだけ使う。
func (g *GadgetService) PomodoroTap(id string, work, brk time.Duration) {
	g.update(func(now time.Time) bool {
		p := g.f.Pomodoro[id]
		v := p.view(now, work)
		switch {
		case v.Alert:
			t := now.UTC()
			p.AckAt = &t
		case p.Running:
			p = p.pauseAt(now)
		default:
			if !v.Started || p.WorkMS <= 0 || p.BreakMS <= 0 {
				p.WorkMS, p.BreakMS = work.Milliseconds(), brk.Milliseconds()
			}
			w, b := time.Duration(p.WorkMS)*time.Millisecond, time.Duration(p.BreakMS)*time.Millisecond
			// 残りに合うよう、今の周期の始まりを決める
			var a time.Time
			done := v.Count
			if v.Phase == pomoPhaseBreak {
				a = now.Add(-(w + b - v.Remaining))
				done--
			} else {
				a = now.Add(-(w - v.Remaining))
			}
			a = a.UTC()
			ack := now.UTC() // 続けたときは、計算の上の切り替わりで点滅させない
			p = PomodoroState{Running: true, Anchor: &a, WorkMS: p.WorkMS, BreakMS: p.BreakMS, Done: max(done, 0), AckAt: &ack}
		}
		p.UpdatedAt = now.UTC()
		g.f.Pomodoro[id] = p
		return true
	})
}

// PomodoroReset は、まだ始めていない状態に戻す（終えた作業の数も 0）。
func (g *GadgetService) PomodoroReset(id string) {
	g.update(func(now time.Time) bool {
		g.f.Pomodoro[id] = PomodoroState{UpdatedAt: now.UTC()}
		return true
	})
}

// scheduleAlertLocked は、動いているポモドーロのうち、いちばん早く段階が切り替わる時刻に onAlert を呼ぶよう決める。
func (g *GadgetService) scheduleAlertLocked(now time.Time) {
	if g.alert != nil {
		g.alert.Stop()
		g.alert = nil
	}
	var next time.Time
	for _, p := range g.f.Pomodoro {
		if v := p.view(now, 0); v.Running {
			if t := now.Add(v.Remaining); next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}
	if next.IsZero() {
		return
	}
	g.alert = time.AfterFunc(time.Until(next)+widgetWakeSlack, func() {
		g.mu.Lock()
		f, cb := g.onAlert, g.onChange
		g.scheduleAlertLocked(time.Now())
		g.mu.Unlock()
		log.Printf("widgets: pomodoro switched")
		if f != nil {
			f()
		}
		if cb != nil {
			cb()
		}
	})
}

// ---------- カウンター ----------

// CounterAdd は値に delta を足す（0 から counterMax まで）。
func (g *GadgetService) CounterAdd(id string, delta int64) {
	g.update(func(now time.Time) bool {
		c := g.f.Counter[id]
		c.Value = min(max(c.Value+delta, 0), counterMax)
		c.UpdatedAt = now.UTC()
		g.f.Counter[id] = c
		return true
	})
}

// CounterReset は 0 に戻す。
func (g *GadgetService) CounterReset(id string) {
	g.update(func(now time.Time) bool {
		g.f.Counter[id] = CounterState{UpdatedAt: now.UTC()}
		return true
	})
}

// ---------- 在席ランプ ----------

// PresenceStep は、states の中で今の状態から dir（+1 で次、-1 で前）だけ進める。
// 今の状態が states になければ、+1 なら最初、-1 なら最後にする。
func (g *GadgetService) PresenceStep(id string, states []PresenceDef, dir int, source string) string {
	var got string
	g.update(func(now time.Time) bool {
		if len(states) == 0 {
			return false
		}
		cur := presenceIndex(states, g.f.Presence[id].State)
		var i int
		switch {
		case cur < 0 && dir > 0:
			i = 0
		case cur < 0:
			i = len(states) - 1
		default:
			i = ((cur+dir)%len(states) + len(states)) % len(states)
		}
		got = states[i].Name
		g.f.Presence[id] = PresenceState{State: got, UpdatedAt: now.UTC(), Source: source}
		return true
	})
	return got
}

// PresenceSet は状態を name にする。
func (g *GadgetService) PresenceSet(id, name, source string) {
	g.update(func(now time.Time) bool {
		g.f.Presence[id] = PresenceState{State: name, UpdatedAt: now.UTC(), Source: source}
		return true
	})
}

func presenceIndex(states []PresenceDef, name string) int {
	for i, s := range states {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// ---------- 時刻合わせ、終了 ----------

// ClockStepped は、時刻合わせでシステムの時刻が off だけ動いたときに呼ぶ。
// 動いているタイマーの始まりの時刻を同じだけ動かし、経過時間を保つ。
func (g *GadgetService) ClockStepped(off time.Duration) {
	g.update(func(now time.Time) bool {
		n := 0
		for _, m := range []map[string]StopwatchState{g.f.Stopwatch, g.f.Presenter} {
			for id, s := range m {
				if s.StartedAt != nil {
					t := s.StartedAt.Add(off)
					s.StartedAt = &t
					m[id] = s
					n++
				}
			}
		}
		for id, p := range g.f.Pomodoro {
			if p.Running && p.Anchor != nil {
				t := p.Anchor.Add(off)
				p.Anchor = &t
				if p.AckAt != nil {
					a := p.AckAt.Add(off)
					p.AckAt = &a
				}
				g.f.Pomodoro[id] = p
				n++
			}
		}
		if n > 0 {
			log.Printf("widgets: shifted %d running timers by %v (clock set)", n, off.Round(time.Millisecond))
		}
		return n > 0
	})
}

// Shutdown は、デーモンが終わるときに呼ぶ。終わった時刻を書き、Brain を再起動したあとでも、そこまでの時間を残す。
func (g *GadgetService) Shutdown() {
	if g == nil {
		return
	}
	now := time.Now().UTC()
	g.mu.Lock()
	running := false
	for _, s := range g.f.Stopwatch {
		running = running || s.StartedAt != nil
	}
	running = running || g.presenterRunningLocked()
	for _, p := range g.f.Pomodoro {
		running = running || p.Running
	}
	if g.alert != nil {
		g.alert.Stop()
	}
	g.f.LastSeen = &now
	g.f.BootID = g.bootID
	f := g.f.clone()
	g.mu.Unlock()
	if !running {
		return
	}
	if err := g.store.Save(gadgetStoreName, f); err != nil {
		log.Printf("widgets: save %s: %v", gadgetStoreName, err)
	}
}

// saver は、変わるたびに widgets.json を書く。書いているあいだの変更は、次の 1 回にまとめる。
func (g *GadgetService) saver() {
	for range g.dirty {
		g.mu.Lock()
		g.f.BootID = g.bootID
		g.f.LastSeen = nil
		f := g.f.clone()
		g.mu.Unlock()
		if err := g.store.Save(gadgetStoreName, f); err != nil {
			log.Printf("widgets: save %s: %v", gadgetStoreName, err)
		}
		if g.saved != nil {
			g.saved <- struct{}{}
		}
	}
}

// ---------- get_widgets ----------

// gadgetIDs は、設定のどこかで kind のウィジェットに使われている id を、名前の順に返す。
func gadgetIDs(cfg *Config, kind string) []string {
	seen := map[string]bool{}
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		for _, a := range l.Touch.Cells {
			if a.Widget == kind {
				seen[widgetStateID(a)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// widgetStateID は、セルの状態の id。id を書かなければ、ウィジェットの種類の名前。
func widgetStateID(a ActionSpec) string {
	if a.ID != "" {
		return a.ID
	}
	return a.Widget
}

// presenceStates は、設定で id の在席ランプに書いた状態を、出てきた順に（重なりを除いて）返す。
func presenceStates(cfg *Config, id string) []PresenceDef {
	var out []PresenceDef
	seen := map[string]bool{}
	for _, l := range cfg.Layers {
		if l.Touch == nil {
			continue
		}
		keys := make([]string, 0, len(l.Touch.Cells))
		for k := range l.Touch.Cells {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a := l.Touch.Cells[k]
			if a.Widget != widgetPresence || widgetStateID(a) != id {
				continue
			}
			states, err := compilePresenceStates(a.States)
			if err != nil {
				continue
			}
			for _, s := range states {
				if !seen[s.Name] {
					seen[s.Name] = true
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func (s PresenceDef) String() string { return fmt.Sprintf("%s(%s)", s.Name, s.Color.hex()) }
