package cloudkit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-reminders/internal/auth"
)

func TestNativeResolutionTokensPreserveReplicaAndUseAppleEpoch(t *testing.T) {
	before := float64(time.Now().UnixMilli()-978307200000) / 1000
	text, err := BumpResolutionTokens(`{"map":{"name":{"counter":4,"replicaID":"existing","modificationTime":123,"future":"keep"},"other":{"counter":9,"modificationTime":456}},"extension":"keep"}`, []string{"name", "name", "new"})
	if err != nil {
		t.Fatal(err)
	}
	var tokens struct {
		Map       map[string]map[string]interface{} `json:"map"`
		Extension string                            `json:"extension"`
	}
	if json.Unmarshal([]byte(text), &tokens) != nil {
		t.Fatal("tokens invalid")
	}
	name := tokens.Map["name"]
	timestamp := name["modificationTime"].(float64)
	if name["counter"] != float64(5) || name["replicaID"] != "existing" || name["future"] != "keep" || tokens.Extension != "keep" {
		t.Fatal("native token identity or extension properties lost")
	}
	if timestamp < before || timestamp > before+1 || tokens.Map["other"]["modificationTime"] != float64(456) {
		t.Fatal("native timestamp epoch or untouched token changed")
	}
	if tokens.Map["new"]["replicaID"] == "" || tokens.Map["new"]["counter"] != float64(1) {
		t.Fatal("new property token missing replica/counter")
	}
}

func TestStructureAssetsNeverSendAccountCookiesAndRejectForeignURLs(t *testing.T) {
	sawCookie := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie") != ""
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewFromSession(&auth.SessionData{CKBaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadStructureAsset(map[string]interface{}{"downloadURL": server.URL + "/asset"}); err != nil {
		t.Fatal(err)
	}
	if sawCookie {
		t.Fatal("account cookies attached to signed asset URL")
	}
	for _, location := range []string{"https://evil.invalid/asset", "https://icloud-content.com.evil.invalid/asset", "http://other.invalid/asset", "https://user:password@cvws.icloud-content.com/asset"} {
		_, err := client.ReadStructureAsset(map[string]interface{}{"downloadURL": location})
		if err == nil || strings.Contains(err.Error(), location) {
			t.Fatal("foreign asset location accepted or leaked")
		}
	}
}

func TestAssetBoundsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", MaxStructureBytes+1)))
	}))
	defer server.Close()
	client, _ := NewFromSession(&auth.SessionData{CKBaseURL: server.URL})
	if _, err := client.ReadStructureAsset(map[string]interface{}{"downloadURL": server.URL}); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.WithContext(ctx).ReadStructureAsset(map[string]interface{}{"downloadURL": server.URL}); err == nil {
		t.Fatal("canceled asset read continued")
	}
}
