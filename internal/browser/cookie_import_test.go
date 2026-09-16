package browser

import "testing"

func TestParseCookieImportCurl(t *testing.T) {
	raw := `curl 'https://example.com/api' -H 'accept: */*' \
  -H 'cookie: session=abc123; theme=dark' --compressed`
	cookies, err := ParseCookieImport(raw, "example.com")
	if err != nil {
		t.Fatalf("ParseCookieImport: %v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2", len(cookies))
	}
	if cookies[0].Name != "session" || cookies[0].Value != "abc123" {
		t.Fatalf("first cookie = %+v", cookies[0])
	}
	if cookies[1].Name != "theme" || cookies[1].Domain != "example.com" {
		t.Fatalf("second cookie = %+v", cookies[1])
	}
}

func TestParseCookieImportBareHeaderAndJSON(t *testing.T) {
	cookies, err := ParseCookieImport("a=1; b=2", "")
	if err != nil || len(cookies) != 2 {
		t.Fatalf("bare header: %v %d", err, len(cookies))
	}
	cookies, err = ParseCookieImport(`[{"name":"x","value":"1","http_only":true}]`, "")
	if err != nil || len(cookies) != 1 || !cookies[0].HTTPOnly {
		t.Fatalf("json: %v %+v", err, cookies)
	}
}

func TestParseCookieImportRejectsGarbage(t *testing.T) {
	if _, err := ParseCookieImport("curl https://example.com -o out.html", ""); err == nil {
		t.Fatal("a cURL command with no cookie header should fail")
	}
	if _, err := ParseCookieImport("", ""); err == nil {
		t.Fatal("empty input should fail")
	}
}
