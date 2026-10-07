package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPickByID(t *testing.T) {
	const d = "/dev/serial/by-id/"
	cases := []struct {
		name  string
		paths []string
		want  string
		err   string
	}{
		{"設定用は -if05（名前の順に関係なく）", []string{d + "usb-SHARP_Brain_0123456789-if05", d + "usb-SHARP_Brain_0123456789-if03"}, d + "usb-SHARP_Brain_0123456789-if05", ""},
		{"ほかの機器は見ない", []string{d + "usb-FTDI_FT232R_A10K-if00-port0", d + "usb-SHARP_Brain_0123456789-if03", d + "usb-SHARP_Brain_0123456789-if05", d + "usb-Arduino_Uno_85-if00"}, d + "usb-SHARP_Brain_0123456789-if05", ""},
		{"番号は数で比べる（-if10 は -if05 より大きい）", []string{d + "usb-SHARP_Brain_0123456789-if10", d + "usb-SHARP_Brain_0123456789-if05"}, d + "usb-SHARP_Brain_0123456789-if10", ""},
		{"コンソール用しかない（古いガジェット）なら選ばない", []string{d + "usb-SHARP_Brain_0123456789-if03"}, "", "コンソール用の usb-SHARP_Brain_0123456789-if03 だけ"},
		{"Brain がない", []string{d + "usb-FTDI_FT232R_A10K-if00-port0"}, "", ""},
		{"何もない", nil, "", ""},
	}
	for _, c := range cases {
		got, err := pickByID(c.paths)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want == "" && !errors.Is(err, errNotFound) {
			t.Errorf("%s: err = %v, want errNotFound", c.name, err)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		}
	}
}

func TestPickUsbmodem(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"設定用は末尾が大きいほう（6）", []string{"/dev/cu.usbmodem01234567894", "/dev/cu.usbmodem01234567896"}, "/dev/cu.usbmodem01234567896"},
		{"ほかの機器（Brain のシリアル番号で始まらない）は見ない", []string{"/dev/cu.usbmodem14201", "/dev/cu.usbmodem7BB180B43", "/dev/cu.usbmodem01234567894", "/dev/cu.usbmodem01234567896"}, "/dev/cu.usbmodem01234567896"},
		{"コンソール用しかないなら選ばない", []string{"/dev/cu.usbmodem01234567894"}, ""},
		{"ほかの機器だけなら選ばない", []string{"/dev/cu.usbmodem14201", "/dev/cu.usbmodem14203"}, ""},
	}
	for _, c := range cases {
		got, err := pickUsbmodem(c.paths)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want == "" && !errors.Is(err, errNotFound) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}
