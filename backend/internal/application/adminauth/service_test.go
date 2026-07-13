package adminauth

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRefreshTokenRotationAndLogout(t *testing.T) {
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewAdminRepository(database), relational.NewAdminSessionRepository(database), security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
	ctx := context.Background()
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	_, tokens, err := service.Login(ctx, "admin", "password123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := service.Refresh(ctx, tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("旧 refresh token 仍可使用: %v", err)
	}
	if err := service.Logout(ctx, rotated.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateAccess(ctx, rotated.AccessToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("注销后的 access token 仍可使用: %v", err)
	}
	if _, err := service.Refresh(ctx, rotated.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("注销后的 refresh token 仍可使用: %v", err)
	}
}

func TestChangePasswordRevokesAllSessions(t *testing.T) {
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewAdminRepository(database), relational.NewAdminSessionRepository(database), security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
	ctx := context.Background()
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	adminValue, tokens, err := service.Login(ctx, "admin", "password123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ChangePassword(ctx, adminValue.ID, "password123", "password456"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateAccess(ctx, tokens.AccessToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("修改密码后的 access token 仍可使用: %v", err)
	}
	if _, err := service.Refresh(ctx, tokens.RefreshToken); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("修改密码后的 refresh token 仍可使用: %v", err)
	}
	if _, _, err := service.Login(ctx, "admin", "password123", "127.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧密码仍可登录: %v", err)
	}
	if _, _, err := service.Login(ctx, "admin", "password456", "127.0.0.1"); err != nil {
		t.Fatalf("新密码无法登录: %v", err)
	}
}

func TestLoginRateLimiterFailureIsEnforced(t *testing.T) {
	service := NewService(nil, nil, security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
	service.SetLoginRateLimiter(rejectingRateLimiter{})
	if _, _, err := service.Login(context.Background(), "admin", "password123", "127.0.0.1"); !errors.Is(err, ErrLoginRateLimited) {
		t.Fatalf("login rate limit error = %v", err)
	}
}

func TestLoginDistinguishesPersistenceFailure(t *testing.T) {
	service := NewService(failingAdminRepository{}, nil, security.NewTokenService("12345678901234567890123456789012"), time.Minute, time.Hour)
	if _, _, err := service.Login(context.Background(), "admin", "password123", "127.0.0.1"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("login persistence error = %v", err)
	}
}

func TestConcurrentRefreshAllowsExactlyOneRotation(t *testing.T) {
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}

	baseSessions := relational.NewAdminSessionRepository(database)
	sessions := newCoordinatedSessionRepository(baseSessions, 2)
	service := NewService(
		relational.NewAdminRepository(database),
		sessions,
		security.NewTokenService("12345678901234567890123456789012"),
		time.Minute,
		time.Hour,
	)
	ctx := context.Background()
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	_, tokens, err := service.Login(ctx, "admin", "password123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	type refreshResult struct {
		tokens Tokens
		err    error
	}
	results := make(chan refreshResult, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			rotated, refreshErr := service.Refresh(ctx, tokens.RefreshToken)
			results <- refreshResult{tokens: rotated, err: refreshErr}
		}()
	}
	close(start)

	var successful Tokens
	successCount := 0
	invalidCount := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successCount++
			successful = result.tokens
		case errors.Is(result.err, ErrInvalidSession):
			invalidCount++
		default:
			t.Fatalf("unexpected refresh error: %v", result.err)
		}
	}
	if successCount != 1 || invalidCount != 1 {
		t.Fatalf("successes = %d, invalid sessions = %d", successCount, invalidCount)
	}
	if _, err := service.Refresh(ctx, successful.RefreshToken); err != nil {
		t.Fatalf("winning refresh token is unusable: %v", err)
	}
}

type coordinatedSessionRepository struct {
	repository.AdminSessionRepository
	mu        sync.Mutex
	remaining int
	ready     chan struct{}
}

type rejectingRateLimiter struct{}

func (rejectingRateLimiter) Allow(context.Context, string, int, time.Time) (bool, error) {
	return false, nil
}

type failingAdminRepository struct{ repository.AdminRepository }

func (failingAdminRepository) GetByUsername(context.Context, string) (admin.Admin, error) {
	return admin.Admin{}, errors.New("database unavailable")
}

func newCoordinatedSessionRepository(base repository.AdminSessionRepository, reads int) *coordinatedSessionRepository {
	return &coordinatedSessionRepository{AdminSessionRepository: base, remaining: reads, ready: make(chan struct{})}
}

func (r *coordinatedSessionRepository) GetByTokenHash(ctx context.Context, tokenHash string) (admin.Session, error) {
	session, err := r.AdminSessionRepository.GetByTokenHash(ctx, tokenHash)
	if err != nil {
		return admin.Session{}, err
	}
	r.mu.Lock()
	if r.remaining > 0 {
		r.remaining--
		if r.remaining == 0 {
			close(r.ready)
		}
	}
	ready := r.ready
	r.mu.Unlock()
	<-ready
	return session, nil
}
