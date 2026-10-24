package auth

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// sessionJar retains cookie scope and expiry, which cookiejar.Cookies omits.
// PCS approval cookies must survive a restart without acquiring a wider scope.
type sessionJar struct {
	jar     *cookiejar.Jar
	mu      sync.Mutex
	records map[string]Cookie
}

func newSessionJar() *sessionJar {
	jar, _ := cookiejar.New(nil)
	return &sessionJar{jar: jar, records: make(map[string]Cookie)}
}

func (j *sessionJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(u) }

func (j *sessionJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(u, cookies)
	now := time.Now()
	for _, c := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		hostOnly := domain == ""
		if hostOnly {
			domain = strings.ToLower(u.Hostname())
		}
		host := strings.ToLower(u.Hostname())
		if host != domain && !strings.HasSuffix(host, "."+domain) {
			continue
		}
		path := c.Path
		if !strings.HasPrefix(path, "/") {
			path = "/"
			if index := strings.LastIndex(u.Path, "/"); index > 0 {
				path = u.Path[:index]
			}
		}
		key := domain + "\x00" + path + "\x00" + c.Name
		expires := c.Expires
		if c.MaxAge > 0 {
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		if c.MaxAge < 0 || (!expires.IsZero() && !expires.After(now)) {
			delete(j.records, key)
			continue
		}
		// Record only cookies actually accepted by the underlying jar.
		probe := *u
		probe.Scheme, probe.Path = "https", path
		accepted := false
		for _, active := range j.jar.Cookies(&probe) {
			if active.Name == c.Name && active.Value == c.Value {
				accepted = true
				break
			}
		}
		if !accepted {
			continue
		}
		stored := Cookie{Name: c.Name, Value: c.Value, Domain: domain, Path: path, Secure: c.Secure, HostOnly: hostOnly}
		if !expires.IsZero() {
			stored.Expires = expires.Unix()
		}
		j.records[key] = stored
	}
}

func (j *sessionJar) snapshot() []Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]Cookie, 0, len(j.records))
	for key, c := range j.records {
		if c.Expires != 0 && c.Expires <= time.Now().Unix() {
			delete(j.records, key)
			continue
		}
		result = append(result, c)
	}
	sort.Slice(result, func(i, k int) bool {
		if result[i].Domain != result[k].Domain {
			return result[i].Domain < result[k].Domain
		}
		if result[i].Path != result[k].Path {
			return result[i].Path < result[k].Path
		}
		return result[i].Name < result[k].Name
	})
	return result
}

// RestoreCookies restores scoped cookies for authentication and CloudKit.
// Older session files omitted domains; keep their established compatibility
// behavior while new snapshots always preserve the original scope.
func RestoreCookies(jar http.CookieJar, cookies []Cookie, ckBase string) {
	for _, c := range cookies {
		httpCookie := &http.Cookie{Name: c.Name, Value: unquoteCookieValue(c.Value), Domain: c.Domain, Path: c.Path, Secure: c.Secure}
		if c.Expires != 0 {
			httpCookie.Expires = time.Unix(c.Expires, 0)
		}
		if c.Domain != "" {
			origin := &url.URL{Scheme: "https", Host: strings.TrimPrefix(c.Domain, "."), Path: "/"}
			if c.HostOnly {
				httpCookie.Domain = ""
			}
			jar.SetCookies(origin, []*http.Cookie{httpCookie})
			continue
		}
		for _, raw := range append(append([]string{}, authDomains...), ckBase) {
			if u, err := url.Parse(raw); err == nil && u.Host != "" {
				jar.SetCookies(u, []*http.Cookie{httpCookie})
			}
		}
	}
}
