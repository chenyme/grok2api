package clientkey

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func TestCreateDistinguishesOmittedLimitsFromExplicitZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := clientkeyapp.NewService(relational.NewClientKeyRepository(database), nil, nil, 60, 5, cipher)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api"))

	assertCreate := func(body string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create response = %d %s", response.Code, response.Body.String())
		}
	}
	assertCreate(`{"name":"defaults"}`)
	assertCreate(`{"name":"unlimited","rpmLimit":0,"maxConcurrent":0}`)

	defaults, total, err := service.List(ctx, 1, 20, "defaults", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(defaults) != 1 {
		t.Fatalf("default key list = %#v, total = %d, err = %v", defaults, total, err)
	}
	if defaults[0].RPMLimit != 60 || defaults[0].MaxConcurrent != 5 {
		t.Fatalf("omitted limits = rpm %d, concurrency %d", defaults[0].RPMLimit, defaults[0].MaxConcurrent)
	}
	unlimited, total, err := service.List(ctx, 1, 20, "unlimited", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(unlimited) != 1 {
		t.Fatalf("unlimited key list = %#v, total = %d, err = %v", unlimited, total, err)
	}
	if unlimited[0].RPMLimit != 0 || unlimited[0].MaxConcurrent != 0 {
		t.Fatalf("explicit zero limits = rpm %d, concurrency %d", unlimited[0].RPMLimit, unlimited[0].MaxConcurrent)
	}
}
