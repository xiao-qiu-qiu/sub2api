// Package chatgptcookies provides a bounded jar for ChatGPT routing cookies.
package chatgptcookies

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	maxCookies     = 32
	maxCookieBytes = 4096
	maxCookieTTL   = 2 * time.Hour
)

type storedCookie struct {
	cookie http.Cookie
	origin url.URL
	added  time.Time
}

// Jar applies normal domain/path/expiry rules, with a fixed cookie budget and TTL.
type Jar struct {
	mu      sync.Mutex
	inner   *cookiejar.Jar
	entries map[string]storedCookie
}

func NewJar() *Jar {
	inner, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &Jar{inner: inner, entries: make(map[string]storedCookie)}
}

func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j == nil || !IsChatGPTURL(u) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.entries == nil {
		j.entries = make(map[string]storedCookie)
	}
	now := time.Now()
	for key, entry := range j.entries {
		if !now.Before(entry.cookie.Expires) {
			delete(j.entries, key)
		}
	}
	for _, incoming := range cookies {
		if incoming == nil || !IsAllowedName(incoming.Name) || len(incoming.Name) > 128 || len(incoming.Value) > maxCookieBytes || len(incoming.Path) > 1024 {
			continue
		}
		cookie := *incoming
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if domain == "" {
			domain = strings.ToLower(u.Hostname())
		}
		if !IsChatGPTHost(domain) || (domain != strings.ToLower(u.Hostname()) && !strings.HasSuffix(strings.ToLower(u.Hostname()), "."+domain)) {
			continue
		}
		if cookie.Path == "" || cookie.Path[0] != '/' {
			cookie.Path = "/"
			if end := strings.LastIndex(u.Path, "/"); end > 0 {
				cookie.Path = u.Path[:end]
			}
		}
		key := domain + "\x00" + cookie.Path + "\x00" + cookie.Name
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(now) && cookie.MaxAge == 0) {
			delete(j.entries, key)
			continue
		}
		expires := now.Add(maxCookieTTL)
		if cookie.MaxAge > 0 && cookie.MaxAge < int(maxCookieTTL/time.Second) {
			expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
		}
		if cookie.MaxAge == 0 && !cookie.Expires.IsZero() && cookie.Expires.Before(expires) {
			expires = cookie.Expires
		}
		cookie.MaxAge, cookie.Expires = 0, expires
		cookie.Secure = true
		if _, exists := j.entries[key]; !exists && len(j.entries) >= maxCookies {
			oldestKey := ""
			var oldest time.Time
			for candidate, entry := range j.entries {
				if oldestKey == "" || entry.added.Before(oldest) {
					oldestKey, oldest = candidate, entry.added
				}
			}
			delete(j.entries, oldestKey)
		}
		origin := *u
		origin.User, origin.RawQuery, origin.Fragment = nil, "", ""
		j.entries[key] = storedCookie{cookie: cookie, origin: origin, added: now}
	}
	// Rebuilding keeps even adversarial domain/path churn within the fixed budget.
	j.inner, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	for _, entry := range j.entries {
		j.inner.SetCookies(&entry.origin, []*http.Cookie{&entry.cookie})
	}
}

func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	if j == nil || !IsChatGPTURL(u) {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.inner == nil {
		return nil
	}
	return j.inner.Cookies(u)
}

func IsChatGPTURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && IsChatGPTHost(u.Hostname())
}

func IsChatGPTHost(host string) bool {
	switch host = strings.ToLower(strings.TrimSpace(host)); host {
	case "chatgpt.com", "chat.openai.com", "chatgpt-staging.com":
		return true
	default:
		return strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
	}
}

func IsAllowedName(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "__oailb", "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}
