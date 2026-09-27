package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGetMetadataCoalescesRequests(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Write([]byte(`{"release_id":7,"code":"TEST-7"}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "test-key", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := client.GetMetadata(context.Background(), 7)
			if err != nil || item == nil || item.Code != "TEST-7" {
				t.Errorf("metadata: %+v, %v", item, err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
