package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Brain には CDC-ACM が 2 つある（gadget-setup.sh）。
//   acm.usb0 → インターフェイス 3（コンソール用。getty がログイン画面を出している）
//   acm.usb1 → インターフェイス 5（設定用。lefthand が答える）
// コンソール用のポートに hello を書くと、ログイン画面にユーザー名として入力されてしまう。
// そのため、ポートを開く前に名前だけで設定用を見分け、コンソール用は開かない。

// brainSerial は gadget-setup.sh の strings/0x409/serialnumber。macOS のデバイス名に入る。
const brainSerial = "0123456789"

var byIDIface = regexp.MustCompile(`^(.*)-if([0-9]+)$`)

// pickByID は、Linux の /dev/serial/by-id/ の名前（usb-SHARP_Brain_<シリアル番号>-if03 と -if05）から、
// 設定用のポートを選ぶ。同じ Brain の ACM のうち、インターフェイス番号がいちばん大きいものが設定用。
// ACM が 1 つしかない（古いガジェット）ときは、それはコンソール用なので選ばない。
func pickByID(paths []string) (string, error) {
	return pickLargest(paths, func(name string) (string, int, bool) {
		if !strings.Contains(name, "Brain") {
			return "", 0, false
		}
		m := byIDIface.FindStringSubmatch(name)
		if m == nil {
			return "", 0, false
		}
		n, _ := strconv.Atoi(m[2])
		return m[1], n, true
	})
}

// pickUsbmodem は、macOS の /dev/cu.usbmodem<シリアル番号><番号> から、設定用のポートを選ぶ。
// 末尾の番号は、ACM の制御インターフェイスの番号に 1 を足したもの（コンソール用は 4、設定用は 6）。
// ほかの機器に書かないよう、Brain のシリアル番号で始まる名前だけを見る。
func pickUsbmodem(paths []string) (string, error) {
	return pickLargest(paths, func(name string) (string, int, bool) {
		rest, ok := strings.CutPrefix(name, "cu.usbmodem"+brainSerial)
		if !ok || rest == "" {
			return "", 0, false
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, false
		}
		return brainSerial, n, true
	})
}

// pickLargest は、parse で（機器、番号）に分けられたポートを機器ごとにまとめ、
// ポートが 2 つ以上ある機器の、番号がいちばん大きいポートを返す。機器が複数あれば、名前の順で最初のもの。
func pickLargest(paths []string, parse func(name string) (dev string, n int, ok bool)) (string, error) {
	type port struct {
		path string
		n    int
	}
	devs := map[string][]port{}
	var seen []string
	for _, p := range paths {
		dev, n, ok := parse(filepath.Base(p))
		if !ok {
			continue
		}
		devs[dev] = append(devs[dev], port{p, n})
		seen = append(seen, filepath.Base(p))
	}
	if len(devs) == 0 {
		return "", errNotFound
	}
	keys := make([]string, 0, len(devs))
	for k := range devs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ps := devs[k]
		if len(ps) < 2 {
			continue
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].n > ps[j].n })
		return ps[0].path, nil
	}
	sort.Strings(seen)
	return "", fmt.Errorf("%w (設定用のポートがありません。見つかったのはコンソール用の %s だけです。Brain のガジェットが古いかもしれません)",
		errNotFound, strings.Join(seen, "、"))
}
