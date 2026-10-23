package relational

import (
	"context"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func openReauthTestRepository(t *testing.T) *AccountRepository {
	t.Helper()
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "reauth-import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return NewAccountRepository(database)
}

// 重导入是内容同步而非凭据更新：已被上游拒绝的账号不得被洗回 active，
// 否则 last_error 仍在的凭据会重新参与路由，面板也会显示为正常。
func TestReimportKeepsReauthRequiredCredential(t *testing.T) {
	ctx := context.Background()
	repo := openReauthTestRepository(t)
	seed := accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web-1",
		UserID: "web-user-1", SourceKey: "web-source-1", EncryptedAccessToken: testEncryptedToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	}
	created, fresh, err := repo.UpsertByIdentity(ctx, seed)
	if err != nil || !fresh {
		t.Fatalf("首次导入 created=%v err=%v", fresh, err)
	}
	rejected := created
	rejected.AuthStatus = accountdomain.AuthStatusReauthRequired
	rejected.LastError = "grok_web SSO credential rejected"
	if _, err := repo.Update(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	marked, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marked.ReauthMarkedAt == nil {
		t.Fatal("标记 reauthRequired 后缺少 reauth_marked_at 锚点")
	}
	seed.EncryptedAccessToken = "encrypted-rotated"
	reimported, fresh, err := repo.UpsertByIdentity(ctx, seed)
	if err != nil || fresh {
		t.Fatalf("重导入 created=%v err=%v", fresh, err)
	}
	if reimported.AuthStatus != accountdomain.AuthStatusReauthRequired {
		t.Fatalf("重导入把 reauthRequired 洗成了 %s", reimported.AuthStatus)
	}
	if reimported.LastError != "grok_web SSO credential rejected" {
		t.Fatalf("重导入改写了 last_error = %q", reimported.LastError)
	}
	if reimported.ReauthMarkedAt == nil || !reimported.ReauthMarkedAt.Equal(*marked.ReauthMarkedAt) {
		t.Fatalf("重导入改写了 reauth_marked_at = %v", reimported.ReauthMarkedAt)
	}
}

// 上游校验成功是 Web SSO 唯一可自动解除 reauth 的信号，解除后账号应恢复可路由状态。
func TestClearReauthMarkRestoresActiveCredential(t *testing.T) {
	ctx := context.Background()
	repo := openReauthTestRepository(t)
	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web-2",
		UserID: "web-user-2", SourceKey: "web-source-2", EncryptedAccessToken: testEncryptedToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected := created
	rejected.AuthStatus = accountdomain.AuthStatusReauthRequired
	rejected.LastError = "grok_web SSO credential rejected"
	if _, err := repo.Update(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	cleared, err := repo.ClearReauthMark(ctx, created.ID)
	if err != nil || !cleared {
		t.Fatalf("校验成功后解除标记 cleared=%v err=%v", cleared, err)
	}
	restored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AuthStatus != accountdomain.AuthStatusActive || restored.LastError != "" || restored.ReauthMarkedAt != nil {
		t.Fatalf("解除标记后状态 = %s / %q / %v", restored.AuthStatus, restored.LastError, restored.ReauthMarkedAt)
	}
	if again, err := repo.ClearReauthMark(ctx, created.ID); err != nil || again {
		t.Fatalf("重复解除标记 cleared=%v err=%v", again, err)
	}
}

// 解除标记只作用于 reauthRequired：正常账号的经营性 last_error 不能被顺手清空。
func TestClearReauthMarkIgnoresActiveCredential(t *testing.T) {
	ctx := context.Background()
	repo := openReauthTestRepository(t)
	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web-3",
		UserID: "web-user-3", SourceKey: "web-source-3", EncryptedAccessToken: testEncryptedToken,
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateHealth(ctx, created.ID, created.Provider, 0, nil, "upstream status 0", false); err != nil {
		t.Fatal(err)
	}
	cleared, err := repo.ClearReauthMark(ctx, created.ID)
	if err != nil || cleared {
		t.Fatalf("对 active 账号解除标记 cleared=%v err=%v", cleared, err)
	}
	current, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.AuthStatus != accountdomain.AuthStatusActive || current.LastError != "upstream status 0" {
		t.Fatalf("active 账号被改写 = %s / %q", current.AuthStatus, current.LastError)
	}
}
