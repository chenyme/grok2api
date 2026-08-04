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
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil)), cipher, nil)
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

// Web/Console adapter 在解析期已按 token 去重：跨文件重复 token 的限额与落库均按去重后数量计，
// 因此「重复 token 占超额」的语义只能用不去重的 Build adapter 覆盖（见后续测试）。
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

// SourceKey 去重不得豁免总量限制：service 层按解析条目数（去重前）累计。
// Console/Web adapter 内部已按 token 去重，无法用重复条目溢出解析数；
// Build adapter 不去重，同 refresh_token 派生相同 SourceKey，故用 Build 覆盖该语义。
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
