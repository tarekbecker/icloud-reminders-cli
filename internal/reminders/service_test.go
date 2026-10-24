package reminders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-reminders/internal/auth"
	"icloud-reminders/internal/cache"
	"icloud-reminders/internal/cloudkit"
	"icloud-reminders/internal/utils"
)

type cloudFixture struct {
	server        *httptest.Server
	directory     string
	mu            sync.Mutex
	records       map[string]map[string]any
	sharedRecords map[string]map[string]any
	writeScopes   []string
	writes        int
	failure       string
	delay         time.Duration
	active        atomic.Int32
	maxActive     atomic.Int32
	tokens        []string
	owner         string
	assets        map[string][]byte
	uploads       int
}

func newCloud(t *testing.T) *cloudFixture {
	t.Helper()
	f := &cloudFixture{directory: t.TempDir(), records: map[string]map[string]any{}, owner: "owner"}
	field := func(value any) any { return map[string]any{"value": value} }
	title := func(text string) any {
		encoded, err := utils.EncodeTitle(text)
		if err != nil {
			t.Fatal(err)
		}
		return field(encoded)
	}
	ref := func(id string) any { return field(map[string]any{"recordName": id}) }
	f.records["List/1"] = map[string]any{"recordName": "List/1", "recordType": "List", "recordChangeTag": "list-tag-1", "fields": map[string]any{"Name": field("Shopping")}}
	f.records["List/2"] = map[string]any{"recordName": "List/2", "recordType": "List", "recordChangeTag": "list-tag-1", "fields": map[string]any{"Name": field("Shopping")}}
	f.records["Reminder/AA1"] = map[string]any{"recordName": "Reminder/AA1", "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": map[string]any{"TitleDocument": title("Buy milk"), "Completed": field(0), "List": ref("List/1")}}
	f.records["Reminder/AA2"] = map[string]any{"recordName": "Reminder/AA2", "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": map[string]any{"TitleDocument": title("Buy bread"), "Completed": field(0), "List": ref("List/2")}}
	f.records["Reminder/BB1"] = map[string]any{"recordName": "Reminder/BB1", "recordType": "Reminder", "recordChangeTag": "tag-1", "fields": map[string]any{"TitleDocument": title("Organic milk"), "Completed": field(1), "List": ref("List/1"), "ParentReminder": ref("Reminder/AA1")}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	data, err := json.Marshal(auth.SessionData{CKBaseURL: f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.directory, "session.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cloudFixture) handle(w http.ResponseWriter, r *http.Request) {
	active := f.active.Add(1)
	defer f.active.Add(-1)
	for previous := f.maxActive.Load(); active > previous; previous = f.maxActive.Load() {
		if f.maxActive.CompareAndSwap(previous, active) {
			break
		}
	}
	if f.delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/asset-download/") {
		data, ok := f.assets[strings.TrimPrefix(r.URL.Path, "/asset-download/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(data)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/asset-upload/") {
		if f.failure == "asset" {
			w.WriteHeader(500)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if f.assets == nil {
			f.assets = map[string][]byte{}
		}
		key := strings.TrimPrefix(r.URL.Path, "/asset-upload/")
		f.assets[key] = data
		f.uploads++
		_ = json.NewEncoder(w).Encode(map[string]any{"singleFile": map[string]any{"receipt": key, "downloadURL": f.server.URL + "/asset-download/" + key, "size": len(data)}})
		return
	}
	recordsByID := f.records
	database := "private"
	if strings.Contains(r.URL.Path, "/shared/") {
		recordsByID, database = f.sharedRecords, "shared"
	}
	if strings.HasSuffix(r.URL.Path, "zones/list") {
		if strings.Contains(r.URL.Path, "/shared/") {
			if f.failure == "shared503" {
				w.WriteHeader(503)
				return
			}
			zones := []any{}
			if f.sharedRecords != nil {
				zones = append(zones, map[string]any{"zoneID": map[string]any{"zoneName": "Reminders", "ownerRecordName": "foreign-owner"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"zones": zones})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"zones": []any{map[string]any{"zoneID": map[string]any{"zoneName": "Reminders", "ownerRecordName": f.owner}}}})
		return
	}
	if strings.HasSuffix(r.URL.Path, "changes/zone") {
		if f.failure == "sync503" {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("private-upstream-body"))
			return
		}
		var input struct {
			Zones []struct {
				SyncToken string `json:"syncToken"`
			} `json:"zones"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if len(input.Zones) > 0 {
			f.tokens = append(f.tokens, input.Zones[0].SyncToken)
		}
		records := []any{}
		for _, record := range recordsByID {
			records = append(records, record)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"zones": []any{map[string]any{"records": records, "syncToken": "delta-token", "moreComing": false}}})
		return
	}
	if strings.HasSuffix(r.URL.Path, "records/lookup") {
		if f.failure == "verification_lookup" && f.writes > 0 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("private-upstream-body"))
			return
		}
		var input struct {
			Records []struct {
				Name string `json:"recordName"`
			} `json:"records"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		values := []any{}
		for _, requested := range input.Records {
			record := recordsByID[requested.Name]
			if record == nil {
				record = map[string]any{"recordName": requested.Name, "serverErrorCode": "UNKNOWN_ITEM", "reason": "private-upstream-body"}
			}
			values = append(values, record)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": values})
		return
	}
	if strings.HasSuffix(r.URL.Path, "assets/upload") {
		var input struct {
			Tokens []map[string]any `json:"tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		tokens := []any{}
		for _, token := range input.Tokens {
			token["url"] = f.server.URL + "/asset-upload/" + fmt.Sprintf("asset-%d", f.uploads)
			tokens = append(tokens, token)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": tokens})
		return
	}
	if !strings.HasSuffix(r.URL.Path, "records/modify") {
		w.WriteHeader(404)
		return
	}
	f.writes++
	if f.failure == "http" {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("private-upstream-body"))
		return
	}
	if f.failure == "empty" {
		_, _ = w.Write([]byte("{}"))
		return
	}
	var input struct {
		Atomic bool `json:"atomic"`
		Zone   struct {
			Owner string `json:"ownerRecordName"`
		} `json:"zoneID"`
		Operations []struct {
			Type   string         `json:"operationType"`
			Record map[string]any `json:"record"`
		} `json:"operations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		w.WriteHeader(400)
		return
	}
	if !input.Atomic {
		w.WriteHeader(400)
		return
	}
	if f.failure == "record" {
		rejected := []any{}
		for _, operation := range input.Operations {
			rejected = append(rejected, map[string]any{"recordName": operation.Record["recordName"], "serverErrorCode": "CONFLICT", "reason": "private-upstream-body"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": rejected})
		return
	}
	f.writeScopes = append(f.writeScopes, database+":"+input.Zone.Owner)
	result := []any{}
	for _, operation := range input.Operations {
		id, _ := operation.Record["recordName"].(string)
		record := recordsByID[id]
		if operation.Type == "create" {
			record = operation.Record
			recordsByID[id] = record
		}
		if record == nil {
			w.WriteHeader(400)
			return
		}
		if operation.Type == "delete" {
			record["deleted"] = true
		} else {
			fields, _ := record["fields"].(map[string]any)
			changes, _ := operation.Record["fields"].(map[string]any)
			for key, value := range changes {
				if key == "ReminderIDs" && f.failure == "ordering_ignored" {
					continue
				}
				if key == "Completed" {
					nested, _ := value.(map[string]any)
					if _, ok := nested["value"].(bool); ok {
						w.WriteHeader(400)
						_, _ = w.Write([]byte("private-upstream-body"))
						return
					}
				}
				fields[key] = value
			}
		}
		record["recordChangeTag"] = fmt.Sprintf("tag-%d", f.writes+1)
		result = append(result, map[string]any{"recordName": id, "recordChangeTag": record["recordChangeTag"]})
	}
	if f.failure == "lost_create_response" {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"records": result})
}

func TestCloudKitLifecycleAndFilters(t *testing.T) {
	f := newCloud(t)
	s := New(f.directory, 5*time.Second)
	ctx := context.Background()
	lists, err := s.ListLists(ctx)
	if err != nil || len(lists.Lists) != 2 || lists.Lists[0].ID != "List/1" {
		t.Fatalf("lists: %+v, %v", lists, err)
	}
	page, err := s.List(ctx, ListInput{Limit: 1})
	if err != nil || page.Total != 2 || len(page.Reminders) != 1 || page.NextOffset == nil || *page.NextOffset != 1 {
		t.Fatalf("page: %+v, %v", page, err)
	}
	page, err = s.List(ctx, ListInput{ListID: "List/1", Query: "MILK", IncludeCompleted: true})
	if err != nil || page.Total != 2 {
		t.Fatalf("filter: %+v, %v", page, err)
	}
	page, err = s.List(ctx, ListInput{ParentID: "Reminder/AA1", IncludeCompleted: true})
	if err != nil || page.Total != 1 || page.Reminders[0].ID != "Reminder/BB1" {
		t.Fatalf("parent: %+v, %v", page, err)
	}
	if _, err := s.Get(ctx, IDInput{ID: "AA"}); PublicError(err).Code != "not_found" {
		t.Fatalf("prefix was accepted: %v", err)
	}
	created, err := s.Create(ctx, CreateInput{Title: "Buy butter", ListID: "List/1", ParentID: "Reminder/AA1", Due: "2026-10-15", Priority: "high", Notes: "unsalted"})
	if err != nil || created.ID == "" || created.Status != "created" {
		t.Fatalf("create: %+v, %v", created, err)
	}
	if _, err := s.Update(ctx, UpdateInput{ID: created.ID, Title: "Buy organic butter", Priority: "none"}); err != nil {
		t.Fatal(err)
	}
	item, err := s.Get(ctx, IDInput{ID: created.ID})
	if err != nil || item.Reminder.Title != "Buy organic butter" || item.Reminder.Priority != 0 || item.Reminder.Notes == nil || *item.Reminder.Notes != "unsalted" {
		t.Fatalf("get: %+v, %v", item, err)
	}
	if _, err := s.Complete(ctx, IDInput{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	writes := f.writes
	if _, err := s.Complete(ctx, IDInput{ID: created.ID}); err != nil || f.writes != writes {
		t.Fatalf("completion not idempotent: %v", err)
	}
	if _, err := s.Delete(ctx, DeleteInput{ID: created.ID}); err == nil || f.writes != writes {
		t.Fatal("delete without confirmation performed IO")
	}
	if _, err := s.Delete(ctx, DeleteInput{ID: created.ID, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, IDInput{ID: created.ID}); err == nil {
		t.Fatal("deleted reminder still exists")
	}
	if _, err := s.Sync(ctx, SyncInput{Full: true}); err != nil {
		t.Fatal(err)
	}
	if f.tokens[len(f.tokens)-1] != "" {
		t.Fatal("full sync reused delta token")
	}
	for _, name := range []string{"session.json", "ck_cache.json"} {
		stat, err := os.Stat(filepath.Join(f.directory, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("private file permissions: %v, %v", stat, err)
		}
	}
}

func TestWritesRejectRemoteErrorsWithoutUpdatingCache(t *testing.T) {
	for _, failure := range []string{"http", "record", "empty"} {
		for _, operation := range []string{"create", "update", "complete", "delete"} {
			t.Run(failure+"/"+operation, func(t *testing.T) {
				f := newCloud(t)
				s := New(f.directory, 5*time.Second)
				if _, err := s.ListLists(context.Background()); err != nil {
					t.Fatal(err)
				}
				before, _ := json.Marshal(cache.LoadFrom(f.directory))
				f.failure = failure
				var err error
				switch operation {
				case "create":
					_, err = s.Create(context.Background(), CreateInput{Title: "new", ListID: "List/1"})
				case "update":
					_, err = s.Update(context.Background(), UpdateInput{ID: "Reminder/AA1", Title: "changed"})
				case "complete":
					_, err = s.Complete(context.Background(), IDInput{ID: "Reminder/AA1"})
				case "delete":
					_, err = s.Delete(context.Background(), DeleteInput{ID: "Reminder/AA1", Confirm: true})
				}
				if err == nil || strings.Contains(err.Error(), "private-upstream-body") {
					t.Fatalf("unsafe or missing error: %v", err)
				}
				after, _ := json.Marshal(cache.LoadFrom(f.directory))
				// UpdatedAt changes on sync; compare the source records separately.
				var old, current map[string]json.RawMessage
				_ = json.Unmarshal(before, &old)
				_ = json.Unmarshal(after, &current)
				if string(old["reminders"]) != string(current["reminders"]) {
					t.Fatal("failed write changed cached reminders")
				}
				if f.writes != 1 {
					t.Fatalf("write was retried: %d", f.writes)
				}
			})
		}
	}
}

func TestNonInteractiveAuthAndValidation(t *testing.T) {
	t.Setenv("ICLOUD_USERNAME", "unused@example.invalid")
	t.Setenv("ICLOUD_PASSWORD", "unused-private-password")
	s := New(t.TempDir(), time.Second)
	if _, err := s.ListLists(context.Background()); err == nil || PublicError(err).Code != "auth_required" {
		t.Fatalf("missing session: %v", err)
	}
	for _, in := range []CreateInput{
		{Title: "text", ListID: "id", Due: "2026-02-30"},
		{Title: "text", ListID: "id", Priority: "urgent"},
		{Title: " ", ListID: "id"},
	} {
		if _, err := s.Create(context.Background(), in); err == nil || PublicError(err).Code != "invalid_argument" {
			t.Fatalf("validation: %v", err)
		}
	}
	if _, err := s.Delete(context.Background(), DeleteInput{ID: "id"}); err == nil || PublicError(err).Code != "invalid_argument" {
		t.Fatal("confirmation was not checked before auth")
	}
	f := newCloud(t)
	f.failure = "sync503"
	if _, err := New(f.directory, time.Second).ListLists(context.Background()); err == nil || strings.Contains(err.Error(), "private-upstream-body") {
		t.Fatalf("503 triggered unsafe authentication/error: %v", err)
	}
}

func TestSerializedCallsAndCancellation(t *testing.T) {
	f := newCloud(t)
	f.delay = 10 * time.Millisecond
	s := New(f.directory, 3*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ListLists(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.maxActive.Load() != 1 {
		t.Fatalf("concurrent iCloud requests: %d", f.maxActive.Load())
	}
	// A queued caller must be able to cancel without waiting for the account.
	<-s.gate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ListLists(ctx); err == nil || PublicError(err).Code != "request_timeout" {
		t.Fatalf("queued cancellation: %v", err)
	}
	s.gate <- struct{}{}
	// Cancellation also propagates to active HTTP requests.
	f.delay = 200 * time.Millisecond
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.ListLists(ctx); err == nil || PublicError(err).Code != "request_timeout" {
		t.Fatalf("active cancellation: %v", err)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatal("cancellation did not reach iCloud HTTP request")
	}
}

func TestAccountChangeDropsPreviousRecordsAndDeltaToken(t *testing.T) {
	f := newCloud(t)
	s := New(f.directory, 5*time.Second)
	if _, err := s.ListLists(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached := cache.LoadFrom(f.directory)
	cached.Reminders["Reminder/old-account"] = &cache.ReminderData{Title: "private previous-account title"}
	if err := cached.Save(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.owner = "another-account"
	f.mu.Unlock()
	page, err := s.List(context.Background(), ListInput{IncludeCompleted: true})
	if err != nil || page.Total != 3 {
		t.Fatalf("account isolation: %+v, %v", page, err)
	}
	for _, item := range page.Reminders {
		if item.ID == "Reminder/old-account" {
			t.Fatal("previous account leaked")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens[len(f.tokens)-1] != "" {
		t.Fatal("new account reused previous delta token")
	}
}

func TestEmptyAccountHasArrayResults(t *testing.T) {
	f := newCloud(t)
	f.records = map[string]map[string]any{}
	s := New(f.directory, 5*time.Second)
	lists, err := s.ListLists(context.Background())
	if err != nil || lists.Lists == nil || len(lists.Lists) != 0 {
		t.Fatalf("empty lists: %+v, %v", lists, err)
	}
	page, err := s.List(context.Background(), ListInput{})
	if err != nil || page.Reminders == nil || page.Total != 0 {
		t.Fatalf("empty page: %+v, %v", page, err)
	}
}

func TestWebAccessDenialIsSeparateFromExpiredAuthentication(t *testing.T) {
	blocked := `{"serverErrorCode":"ACCESS_DENIED","reason":"private db access disabled for this account","uuid":"private-request-id"}`
	for _, err := range []error{
		auth.ErrWebAccessDisabled,
		fmt.Errorf("owner lookup: %w", &cloudkit.APIError{StatusCode: 403, Body: blocked}),
	} {
		public := PublicError(err)
		if public.Code != "icloud_access_denied" || strings.Contains(public.Error(), "private-request-id") {
			t.Fatal("blocked access should give safe web-access guidance rather than requesting another login")
		}
	}
	for _, api := range []*cloudkit.APIError{
		{StatusCode: 401, Body: blocked},
		{StatusCode: 403, Body: `{"serverErrorCode":"AUTHENTICATION_FAILED","reason":"private db access disabled for this account"}`},
		{StatusCode: 403, Body: `{"serverErrorCode":"ACCESS_DENIED","reason":"session expired"}`},
		{StatusCode: 403, Body: "invalid JSON"},
	} {
		if PublicError(api).Code != "auth_required" {
			t.Fatal("unrecognized authorization failures must still require authentication")
		}
	}
}
