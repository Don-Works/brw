package browser

import "testing"

func TestNormalizeLocaleAcceptsBCP47(t *testing.T) {
	cfg, clear, err := NormalizeLocale(LocaleOptions{Locale: "en-GB", Timezone: "Europe/London"})
	if err != nil || clear {
		t.Fatalf("NormalizeLocale: cfg=%+v clear=%v err=%v", cfg, clear, err)
	}
	if cfg.Locale != "en-GB" || cfg.LocaleICU != "en_GB" || cfg.Timezone != "Europe/London" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestNormalizeLocaleRejectsJunk(t *testing.T) {
	if _, _, err := NormalizeLocale(LocaleOptions{}); err == nil {
		t.Fatal("empty request succeeded")
	}
	if _, _, err := NormalizeLocale(LocaleOptions{Locale: "en\nGB"}); err == nil {
		t.Fatal("newline locale succeeded")
	}
	if _, _, err := NormalizeLocale(LocaleOptions{Timezone: "London"}); err == nil {
		t.Fatal("bare city timezone succeeded")
	}
}

func TestNormalizeLocaleClear(t *testing.T) {
	_, clear, err := NormalizeLocale(LocaleOptions{Clear: true})
	if err != nil || !clear {
		t.Fatalf("clear: err=%v clear=%v", err, clear)
	}
}
