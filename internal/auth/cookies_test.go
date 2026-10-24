package auth

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"testing"
	"time"
)

func TestApprovalCookiesKeepHostPathExpiryAndSecureScopeAfterRestore(t *testing.T) {
	jar := newSessionJar()
	origin, _ := url.Parse("https://setup.icloud.com/setup/ws/1/requestPCS")
	jar.SetCookies(origin, []*http.Cookie{
		{Name: "pcs", Value: "private", Domain: ".icloud.com", Path: "/", Secure: true, MaxAge: 600},
		{Name: "host", Value: "private", Path: "/", Secure: true},
		{Name: "path", Value: "private", Domain: ".icloud.com", Path: "/setup", Secure: true},
	})
	stored := jar.snapshot()
	restored, _ := cookiejar.New(nil)
	RestoreCookies(restored, stored, "https://p1-ckdatabasews.icloud.com")
	for _, tc := range []struct {
		url   string
		names []string
	}{
		{"https://p1-ckdatabasews.icloud.com/", []string{"pcs"}},
		{"https://setup.icloud.com/", []string{"host", "pcs"}},
		{"https://setup.icloud.com/setup/test", []string{"host", "path", "pcs"}},
		{"https://sub.setup.icloud.com/", []string{"pcs"}},
		{"https://idmsa.apple.com/", nil},
		{"http://setup.icloud.com/", nil},
	} {
		u, _ := url.Parse(tc.url)
		var names []string
		for _, c := range restored.Cookies(u) {
			names = append(names, c.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, tc.names) {
			t.Fatal("restoring approval changed cookie scope")
		}
	}
	for _, c := range stored {
		if c.Name == "pcs" && (c.Expires <= time.Now().Unix() || c.Expires > time.Now().Add(601*time.Second).Unix()) {
			t.Fatal("max-age expiry was not retained")
		}
	}
}

func TestDeletedExpiredAndRejectedCookiesAreNotPersisted(t *testing.T) {
	jar := newSessionJar()
	u, _ := url.Parse("https://setup.icloud.com/")
	jar.SetCookies(u, []*http.Cookie{{Name: "pcs", Value: "private", Domain: ".icloud.com", Path: "/"}})
	jar.SetCookies(u, []*http.Cookie{{Name: "pcs", Value: "private", Domain: ".example.com", Path: "/"}})
	if len(jar.snapshot()) != 1 {
		t.Fatal("rejected cookie domain should not be persisted")
	}
	jar.SetCookies(u, []*http.Cookie{
		{Name: "pcs", Domain: ".icloud.com", Path: "/", MaxAge: -1},
		{Name: "expired", Value: "private", Path: "/", Expires: time.Now().Add(-time.Hour)},
	})
	if len(jar.snapshot()) != 0 {
		t.Fatal("deleted or expired approval cookies must not survive a restart")
	}
}
