package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 4 時間で 4100 mV から 3500 mV まで下がった記録。最後を 0%、外したときを 100% とする
func syntheticBatteryLog(withPoweroff bool) string {
	var b strings.Builder
	b.WriteString("time,uptime_s,vbatt_mv,vbatt_avg_mv,vbus_mv,percent,plugged,backlight,event\n")
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	fmt.Fprintf(&b, "%s,10,4170,4170,4950,100,true,7,start\n", t0.Add(-time.Minute).Format(time.RFC3339))
	n := 4 * 120
	for i := 0; i <= n; i++ {
		mv := 4100 - 600*i/n
		ev := ""
		if i == 0 {
			ev = "unplugged"
		}
		fmt.Fprintf(&b, "%s,%d,%d,%d,40,50,false,3,%s\n", t0.Add(time.Duration(i)*30*time.Second).Format(time.RFC3339), 60+30*i, mv, mv, ev)
	}
	if withPoweroff {
		fmt.Fprintf(&b, "%s,0,0,0,0,-1,false,3,poweroff\n", t0.Add(time.Duration(n)*30*time.Second).Format(time.RFC3339))
	}
	return b.String()
}

func TestFitBatteryCurve(t *testing.T) {
	out, err := fitBatteryCurve(syntheticBatteryLog(true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "battery:\n  curve: [[3500, 0], [3530, 5], [3560, 10], [3620, 20]") || !strings.HasSuffix(out, "[4100, 100]]") ||
		strings.Contains(out, "poweroff がありません") || !strings.Contains(out, "4h0m0s") {
		t.Fatalf("fit:\n%s", out)
	}
	out, _ = fitBatteryCurve(syntheticBatteryLog(false))
	if !strings.Contains(out, "poweroff がありません") {
		t.Fatalf("no poweroff:\n%s", out)
	}
	if _, err := fitBatteryCurve("time,uptime_s,vbatt_mv,vbatt_avg_mv,vbus_mv,percent,plugged,backlight,event\n"); err == nil || !strings.Contains(err.Error(), "unplugged") {
		t.Fatalf("empty: %v", err)
	}
}

func TestStatusRTCAndBatteryLog(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newFakeBrain(t)
	rtcTime := time.Now().Add(-(5*24*time.Hour + 18*time.Hour))
	f.handler = func(req map[string]any) (any, bool) {
		switch req["cmd"] {
		case "get_status":
			return map[string]any{"time": map[string]any{"now": time.Now().Format(time.RFC3339Nano), "synced": true, "boot_source": "rtc"},
				"rtc":  map[string]any{"mode": "read", "readable": true, "time": rtcTime.Format(time.RFC3339), "offset_ms": -(5*24 + 18) * 3600 * 1000, "boot": "not used: behind"},
				"link": map[string]any{"state": "configured", "online": true}}, true
		case "get_battery_log":
			if req["lines"] != nil && req["lines"] != float64(3) {
				return fakeErr{Code: "bad_request", Message: "lines"}, true
			}
			return map[string]any{"csv": syntheticBatteryLog(true)}, true
		}
		return nil, false
	}
	go f.serve([]string{"hello", "get_status", "get_battery_log"})
	code, out, errs := runCLI(t, "--port", f.slave, "status")
	if code != exitOK || !strings.Contains(out, "合わせてある：内蔵 RTC") || !strings.Contains(out, "内蔵 RTC\t") ||
		!strings.Contains(out, "PC との差 −5日18h0m0s、読むだけ") || !strings.Contains(out, "起動したとき：not used: behind") ||
		!strings.Contains(out, "PC とのつながり\tPC とつながっている") {
		t.Fatalf("status: %d\n%s\n%s", code, out, errs)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "battery", "log", "-n", "3"); code != exitOK || !strings.HasPrefix(out, "time,uptime_s") {
		t.Fatalf("log: %d %s", code, out)
	}
	if code, out, _ := runCLI(t, "--port", f.slave, "battery", "log", "--fit"); code != exitOK || !strings.Contains(out, "curve: [[3500, 0]") {
		t.Fatalf("fit: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "battery"); code != exitUsage {
		t.Fatal("battery without log")
	}
	if code, _, _ := runCLI(t, "--port", f.slave, "status", "--fit"); code != exitUsage {
		t.Fatal("--fit with status")
	}
}
