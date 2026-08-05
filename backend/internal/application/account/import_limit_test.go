package account

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// consoleEntries 生成逗号拼接的 Console 账号对象字面量，供裸数组与 accounts 包装形态混合构造。
func consoleEntries(start, count int, token func(index int) string) string {
	var builder strings.Builder
	for index := start; index < start+count; index++ {
		if index > start {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `{"sso_token":%q}`, token(index))
	}
	return builder.String()
}

func distinctConsoleToken(index int) string { return fmt.Sprintf("token-%d", index) }

func newConsoleImportService(t *testing.T) (*Service, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "import-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil, nil)), cipher, nil)
	return service, accounts
}

func countConsoleAccounts(t *testing.T, accounts *relational.AccountRepository) int64 {
	t.Helper()
	_, total, err := accounts.List(context.Background(), repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 1}, Filter: repository.AccountListFilter{Provider: string(accountdomain.ProviderConsole)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// 跨文件聚合边界：裸数组 + accounts 包装混排，合计恰好触及 maxCredentialImportAccounts 时允许导入。
func TestImportConsoleDocumentsAcceptsExactlyAtAggregateLimitAcrossFiles(t *testing.T) {
	service, accounts := newConsoleImportService(t)
	half := maxCredentialImportAccounts / 2
	rest := maxCredentialImportAccounts - half
	arrayDoc := []byte("[" + consoleEntries(0, half, distinctConsoleToken) + "]")
	wrapperDoc := []byte(`{"provider":"grok_console","accounts":[` + consoleEntries(half, rest, distinctConsoleToken) + "]}")

	result, err := service.ImportConsoleCredentialDocumentsWithProgress(context.Background(), [][]byte{arrayDoc, wrapperDoc}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != maxCredentialImportAccounts || countConsoleAccounts(t, accounts) != int64(maxCredentialImportAccounts) {
		t.Fatalf("result = %#v, stored = %d", result, countConsoleAccounts(t, accounts))
	}
}

// 单文件均未超限、跨文件合计超限（max/2 + 其余+1）时：整批 ErrImportLimit，且不得有任何部分写入。
func TestImportConsoleDocumentsRejectsAggregateOverflowWithoutPartialWrites(t *testing.T) {
	service, accounts := newConsoleImportService(t)
	half := maxCredentialImportAccounts / 2
	rest := maxCredentialImportAccounts - half + 1
	arrayDoc := []byte("[" + consoleEntries(0, half, distinctConsoleToken) + "]")
	wrapperDoc := []byte(`{"provider":"grok_console","accounts":[` + consoleEntries(half, rest, distinctConsoleToken) + "]}")

	_, err := service.ImportConsoleCredentialDocumentsWithProgress(context.Background(), [][]byte{arrayDoc, wrapperDoc}, nil, nil)
	if !errors.Is(err, ErrImportLimit) {
		t.Fatalf("error = %v, want import limit", err)
	}
	if stored := countConsoleAccounts(t, accounts); stored != 0 {
		t.Fatalf("expected zero persisted accounts on aggregate overflow, got %d", stored)
	}
}

// Web/Console adapter 在单个文件内按 token 去重，service 再按 SourceKey
// 对跨文件结果去重。本用例验证最终落库数量，不代表重复项可豁免解析总量限制。
func TestImportConsoleDocumentsCountDeduplicatedTokens(t *testing.T) {
	service, accounts := newConsoleImportService(t)
	duplicateDoc := []byte(`[{"sso_token":"shared"},{"sso_token":"shared"}]`)
	sameAgainDoc := []byte(`[{"sso_token":"shared"},{"sso_token":"fresh"}]`)

	result, err := service.ImportConsoleCredentialDocumentsWithProgress(context.Background(), [][]byte{duplicateDoc, sameAgainDoc}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 2 || countConsoleAccounts(t, accounts) != 2 {
		t.Fatalf("result = %#v, stored = %d, want deduplicated count 2", result, countConsoleAccounts(t, accounts))
	}
}

// 跨文件去重发生在 parsedAccounts 累计之后，因此第二个文件即使只重复已有
// SourceKey，也必须计入解析总量限制，且超限时整批不得写入。
func TestImportConsoleDocumentsCountsCrossFileDuplicatesTowardLimit(t *testing.T) {
	service, accounts := newConsoleImportService(t)
	fullDoc := []byte("[" + consoleEntries(0, maxCredentialImportAccounts, distinctConsoleToken) + "]")
	duplicateDoc := []byte(`[{"sso_token":"token-0"}]`)

	_, err := service.ImportConsoleCredentialDocumentsWithProgress(context.Background(), [][]byte{fullDoc, duplicateDoc}, nil, nil)
	if !errors.Is(err, ErrImportLimit) {
		t.Fatalf("error = %v, want import limit", err)
	}
	if stored := countConsoleAccounts(t, accounts); stored != 0 {
		t.Fatalf("expected zero persisted accounts on aggregate overflow, got %d", stored)
	}
}

// SourceKey 去重不得豁免总量限制：service 层按解析条目数（去重前）累计。
// Build adapter 不在解析阶段去重，同 refresh_token 派生相同 SourceKey，
// 本用例覆盖单个文件内部的重复来源同样计入限制。
func TestImportBuildDocumentsCountsDuplicateSourcesTowardLimit(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "import-limit-build.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(cliprovider.NewAdapter(cliprovider.Config{}, cipher)), cipher, nil)

	duplicateEntry := `{"refresh_token":"same-refresh"}`
	duplicateDoc := []byte("[" + strings.Repeat(duplicateEntry+",", maxCredentialImportAccounts-2) + duplicateEntry + "]")
	freshDoc := []byte(`[{"refresh_token":"fresh-1"},{"refresh_token":"fresh-2"}]`)

	_, err = service.ImportCredentialDocumentsWithProgress(ctx, [][]byte{duplicateDoc, freshDoc}, nil, nil)
	if !errors.Is(err, ErrImportLimit) {
		t.Fatalf("error = %v, want import limit", err)
	}
	_, total, listErr := accounts.List(ctx, repository.AccountListQuery{
		Page: repository.PageQuery{Limit: 1}, Filter: repository.AccountListFilter{Provider: string(accountdomain.ProviderBuild)},
	})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if total != 0 {
		t.Fatalf("expected zero persisted accounts, got %d", total)
	}
}
