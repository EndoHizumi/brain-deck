package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------- 電池 ----------

func TestVoltagePercent(t *testing.T) {
	for mv, want := range map[int]int{3000: 0, 3400: 0, 3450: 2, 3700: 33, 3725: 39, 4150: 100, 4300: 100} {
		if got := voltagePercent(mv); got != want {
			t.Errorf("%d mV = %d%%, want %d", mv, got, want)
		}
	}
}

func writeSys(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, v := range files {
		f := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBatteryLRADC(t *testing.T) {
	root := t.TempDir()
	d := "bus/iio/devices/iio:device0/"
	// 2026-10-09 に Brain で読んだ値（USB で PC につないだとき）
	writeSys(t, root, map[string]string{d + "name": lradcName,
		d + "in_voltage7_raw": "2309", d + "in_voltage7_scale": "1.806640625",
		d + "in_voltage15_raw": "2736", d + "in_voltage15_scale": "1.806640625"})
	bs := NewBatteryService(root)
	close(bs.stop)
	in := bs.Info()
	if in == nil || in.VoltageMV != 4172 || !in.Plugged || in.Charging || in.Percent != 100 || in.Source != "lradc" {
		t.Fatalf("info = %+v", in)
	}
	// USB を外した（5V がない）、電圧が下がった
	writeSys(t, root, map[string]string{d + "in_voltage15_raw": "40", d + "in_voltage7_raw": "2048"})
	changed := make(chan struct{}, 1)
	bs.SetOnChange(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	bs.poll()
	in = bs.Info()
	if in.Plugged || in.Charging || in.VoltageMV != 3700 || in.Percent != 33 {
		t.Fatalf("unplugged = %+v (the average must restart when unplugged)", in)
	}
	select {
	case <-changed:
	default:
		t.Error("no onChange")
	}
	// 平均
	writeSys(t, root, map[string]string{d + "in_voltage7_raw": "2076"}) // 3750 mV
	bs.poll()
	if in := bs.Info(); in.VoltageMV != 3725 {
		t.Errorf("average = %d", in.VoltageMV)
	}
	// つないで、満充電より低い
	writeSys(t, root, map[string]string{d + "in_voltage15_raw": "2736", d + "in_voltage7_raw": "2214"}) // 4000 mV
	bs.poll()
	if in := bs.Info(); !in.Plugged || !in.Charging || in.Percent != 85 {
		t.Errorf("charging = %+v", in)
	}
}

func TestBatteryPowerSupply(t *testing.T) {
	root := t.TempDir()
	writeSys(t, root, map[string]string{"class/power_supply/BAT0/type": "Battery", "class/power_supply/BAT0/capacity": "57",
		"class/power_supply/BAT0/status": "Charging", "class/power_supply/AC/type": "Mains", "class/power_supply/AC/online": "1"})
	bs := NewBatteryService(root)
	close(bs.stop)
	if in := bs.Info(); in == nil || in.Percent != 57 || !in.Charging || !in.Plugged || in.Source != "power_supply" {
		t.Fatalf("info = %+v", in)
	}
	if bs := NewBatteryService(t.TempDir()); bs.Info() != nil {
		t.Error("no battery must give nil")
	}
}
