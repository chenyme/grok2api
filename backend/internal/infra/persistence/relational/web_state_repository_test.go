package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestWebResponseStateAndMediaJobRoundTrip(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	accountValue, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierBasic,
		Name: "web", SourceKey: "web", EncryptedAccessToken: testEncryptedToken, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "key", Prefix: "prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	responses := NewResponseRepository(database)
	state := inferencedomain.WebResponseState{ResponseID: "resp_web", AccountID: accountValue.ID, ConversationID: "conv", UpstreamParentResponseID: "parent", ResponseJSON: `{}`, Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := responses.SaveWebState(ctx, state); err != nil {
		t.Fatal(err)
	}
	stored, err := responses.GetWebState(ctx, state.ResponseID, now)
	if err != nil || stored.ConversationID != "conv" || stored.UpstreamParentResponseID != "parent" {
		t.Fatalf("state = %#v, err = %v", stored, err)
	}

	jobs := NewMediaJobRepository(database)
	job := mediadomain.Job{ID: "video_test", RequestID: "request-video-test", ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: accountValue.ID, AccountName: accountValue.Name, Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "test", Seconds: 8, Size: "16:9", Quality: "720p", Status: mediadomain.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	recoverable, err := jobs.ListRecoverableMediaJobs(ctx, 10)
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != job.ID {
		t.Fatalf("jobs = %#v, err = %v", recoverable, err)
	}
	claimed, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_token_0000000000000001")
	if err != nil || !ok || claimed.Status != mediadomain.StatusInProgress || claimed.LeaseUntil == nil {
		t.Fatalf("claimed job = %#v, ok = %v, err = %v", claimed, ok, err)
	}
	firstClaim := claimed
	if _, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now.Add(time.Minute), now.Add(2*time.Hour), "claim_token_0000000000000002"); err != nil || ok {
		t.Fatalf("active lease was claimed again: ok = %v, err = %v", ok, err)
	}
	claimed, ok, err = jobs.TryClaimMediaJob(ctx, job.ID, now.Add(2*time.Hour), now.Add(3*time.Hour), "claim_token_0000000000000003")
	if err != nil || !ok || claimed.LeaseUntil == nil || !claimed.LeaseUntil.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("expired job claim = %#v, ok = %v, err = %v", claimed, ok, err)
	}
	firstClaim.Progress = 50
	if err := jobs.UpdateMediaJob(ctx, firstClaim); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale claim update error = %v", err)
	}
	completedAt := now.Add(2*time.Hour + time.Minute)
	claimed.Status, claimed.Progress, claimed.LeaseUntil, claimed.CompletedAt = mediadomain.StatusCompleted, 100, nil, &completedAt
	if err := jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	unrecorded, err := jobs.ListUnrecordedTerminalMediaJobs(ctx, 10)
	if err != nil || len(unrecorded) != 1 || unrecorded[0].RequestID != job.RequestID || unrecorded[0].ModelRouteID != job.ModelRouteID {
		t.Fatalf("unrecorded jobs = %#v, err = %v", unrecorded, err)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, job.ID, completedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	unrecorded, err = jobs.ListUnrecordedTerminalMediaJobs(ctx, 10)
	if err != nil || len(unrecorded) != 0 {
		t.Fatalf("recorded jobs = %#v, err = %v", unrecorded, err)
	}
	failedAt := completedAt.Add(2 * time.Second)
	failed := job
	failed.ID, failed.RequestID, failed.Status, failed.ErrorCode = "video_failed", "request-video-failed", mediadomain.StatusFailed, "generation_failed"
	failed.Progress, failed.ClaimToken, failed.CompletedAt, failed.UpdatedAt = 50, "", &failedAt, failedAt
	if err := jobs.CreateMediaJob(ctx, failed); err != nil {
		t.Fatal(err)
	}
	unrecorded, err = jobs.ListUnrecordedTerminalMediaJobs(ctx, 10)
	if err != nil || len(unrecorded) != 1 || unrecorded[0].ID != failed.ID {
		t.Fatalf("failed unrecorded jobs = %#v, err = %v", unrecorded, err)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, failed.ID, failedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}
