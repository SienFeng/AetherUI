package main

import "testing"

func TestParseSettingFlagsDistinguishesUnsetFromEmpty(t *testing.T) {
	t.Run("未传 -listen 时为 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-port", "8080"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen != nil {
			t.Fatalf("未传 -listen，期望 nil，实际 %q", *f.Listen)
		}
		if f.Port != 8080 {
			t.Fatalf("port 期望 8080，实际 %d", f.Port)
		}
	})

	t.Run("传 -listen 空串时非 nil", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", ""})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil {
			t.Fatal("传了 -listen \"\"，期望非 nil（救援用：清空监听地址）")
		}
		if *f.Listen != "" {
			t.Fatalf("期望空串，实际 %q", *f.Listen)
		}
	})

	t.Run("传 -listen 有值", func(t *testing.T) {
		f, err := parseSettingFlags([]string{"-listen", "127.0.0.1", "-basepath", "/Ab3xK9pQ/"})
		if err != nil {
			t.Fatalf("parseSettingFlags: %v", err)
		}
		if f.Listen == nil || *f.Listen != "127.0.0.1" {
			t.Fatalf("listen 期望 127.0.0.1，实际 %v", f.Listen)
		}
		if f.BasePath == nil || *f.BasePath != "/Ab3xK9pQ/" {
			t.Fatalf("basepath 期望 /Ab3xK9pQ/，实际 %v", f.BasePath)
		}
	})
}
