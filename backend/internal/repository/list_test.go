package repository

import "testing"

func TestNormalizePageBoundsAdminPageSize(t *testing.T) {
	tests := []struct {
		name               string
		pageSize           int
		wantPage, wantSize int
	}{
		{name: "invalid page and size", pageSize: 0, wantPage: 1, wantSize: DefaultPageSize},
		{name: "supported large page", pageSize: 2000, wantPage: 1, wantSize: 2000},
		{name: "caps oversized page", pageSize: 5000, wantPage: 1, wantSize: MaxPageSize},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, size := NormalizePage(0, test.pageSize, DefaultPageSize)
			if page != test.wantPage || size != test.wantSize {
				t.Fatalf("NormalizePage() = (%d, %d), want (%d, %d)", page, size, test.wantPage, test.wantSize)
			}
		})
	}
}

func TestNormalizePageSanitizesInvalidDefault(t *testing.T) {
	_, size := NormalizePage(1, 0, MaxPageSize+1)
	if size != DefaultPageSize {
		t.Fatalf("NormalizePage() default size = %d, want %d", size, DefaultPageSize)
	}
}
