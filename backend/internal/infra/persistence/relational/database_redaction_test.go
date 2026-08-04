package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenPostgresInvalidDSNDoesNotLeakCredentials(t *testing.T) {
	const dsn = "postgres://database-user:highly-secret%zz@postgres.internal/grok2api"
	_, err := OpenPostgres(context.Background(), dsn, 1, 1)
	if err == nil {
		t.Fatal("invalid PostgreSQL DSN was accepted")
	}
	if strings.Contains(err.Error(), "highly-secret") || strings.Contains(err.Error(), dsn) {
		t.Fatalf("OpenPostgres error leaked PostgreSQL credentials: %v", err)
	}
}

func TestRedactPostgresErrorMessageRemovesURLCredentials(t *testing.T) {
	const dsn = "postgres://database-user:highly-secret@postgres.internal:5432/grok2api?sslmode=require"
	message := redactPostgresErrorMessage(errors.New("cannot parse or connect to "+dsn), dsn)
	if strings.Contains(message, "highly-secret") || strings.Contains(message, dsn) {
		t.Fatalf("redacted error leaked PostgreSQL credentials: %s", message)
	}
}

func TestRedactPostgresErrorMessageRemovesDriverRenderedPasswords(t *testing.T) {
	tests := []string{
		"connect postgres://database-user:rendered-secret@postgres.internal/grok2api failed",
		"connect failed host=postgres.internal user=database-user password=rendered-secret dbname=grok2api",
		"connect failed host=postgres.internal password='rendered secret' dbname=grok2api",
	}
	for _, input := range tests {
		message := redactPostgresErrorMessage(errors.New(input), "postgres://different:redacted@other.internal/db")
		if strings.Contains(message, "rendered-secret") || strings.Contains(message, "rendered secret") {
			t.Fatalf("redacted error leaked PostgreSQL password: %s", message)
		}
	}
}

func TestPostgresConnectionErrorPreservesErrorChainWithoutLeakingDSN(t *testing.T) {
	sentinel := errors.New("connection sentinel")
	wrapped := &postgresConnectionError{
		operation: "打开 PostgreSQL",
		err:       sentinel,
		dsn:       "postgres://database-user:highly-secret@postgres.internal/grok2api",
	}
	if !errors.Is(wrapped, sentinel) {
		t.Fatal("redacted PostgreSQL error did not preserve its error chain")
	}
	if strings.Contains(wrapped.Error(), "highly-secret") {
		t.Fatalf("wrapped error leaked PostgreSQL password: %s", wrapped)
	}
}
