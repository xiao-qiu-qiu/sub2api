package chatgptcookies

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestJarFiltersHostsNamesPathsAndBounds(t *testing.T) {
	parse := func(raw string) *url.URL { u, _ := url.Parse(raw); return u }
	u := parse("https://chatgpt.com/backend-api/codex/responses")
	jar := NewJar()
	jar.SetCookies(u, []*http.Cookie{
		{Name: "__oailb", Value: "route", Path: "/backend-api", MaxAge: 10},
		{Name: "session-token", Value: "never-store", Path: "/"},
		{Name: "cf_clearance", Value: "bad-domain", Domain: "evil.test", Path: "/"},
	})
	if cookies := jar.Cookies(u); len(cookies) != 1 || cookies[0].Name != "__oailb" {
		t.Fatalf("unexpected cookies: %v", cookies)
	}
	for _, raw := range []string{"http://chatgpt.com/backend-api", "wss://chatgpt.com/backend-api", "https://evilchatgpt.com/backend-api", "https://chatgpt.com.evil.test/backend-api", "https://api.openai.com/backend-api", "https://chatgpt.com:8443/backend-api", "https://chatgpt.com/elsewhere"} {
		if got := jar.Cookies(parse(raw)); len(got) != 0 {
			t.Fatalf("cookies escaped to %s", raw)
		}
	}
	jar.SetCookies(parse("https://evil.test/"), []*http.Cookie{{Name: "__cf_bm", Value: "bad", Domain: "chatgpt.com", Path: "/"}})
	if got := jar.Cookies(u); len(got) != 1 {
		t.Fatalf("untrusted source stored cookie: %v", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "__oailb", Path: "/backend-api", MaxAge: -1}})
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatal("cookie deletion ignored")
	}
	for i := 0; i < maxCookies+5; i++ {
		jar.SetCookies(u, []*http.Cookie{{Name: fmt.Sprintf("cf_chl_%d", i), Value: "v", Path: "/", MaxAge: 9999999}})
	}
	if len(jar.entries) != maxCookies {
		t.Fatalf("unbounded jar: %d", len(jar.entries))
	}
	for _, entry := range jar.entries {
		if time.Until(entry.cookie.Expires) > maxCookieTTL {
			t.Fatal("cookie exceeded TTL")
		}
	}
}
