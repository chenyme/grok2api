package account

import "testing"

func TestWebQuotaRefreshDeduplicatesPerMode(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil)
	service.QueueWebQuotaRefresh(42, "fast")
	service.QueueWebQuotaRefresh(42, "expert")
	service.QueueWebQuotaRefresh(42, "fast")

	service.quotaRefreshMu.Lock()
	defer service.quotaRefreshMu.Unlock()
	if len(service.quotaRefreshes) != 2 {
		t.Fatalf("refresh states = %#v", service.quotaRefreshes)
	}
	if !service.quotaRefreshes["42:fast"].pending {
		t.Fatal("duplicate fast refresh was not marked pending")
	}
	if service.quotaRefreshes["42:expert"].pending {
		t.Fatal("independent expert refresh was incorrectly marked pending")
	}
	if len(service.quotaRefreshQueue) != 2 {
		t.Fatalf("queued refreshes = %d", len(service.quotaRefreshQueue))
	}
}
