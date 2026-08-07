package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type operationsConfigRepositoryStub struct {
	OperationsRepository
	config      domain.OperationsConfig
	getErr      error
	saved       domain.OperationsConfig
	saveCalls   int
	syncUpdates int
}

func (r *operationsConfigRepositoryStub) GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error) {
	if r.getErr != nil {
		return domain.OperationsConfig{}, r.getErr
	}
	return r.config, nil
}

func (r *operationsConfigRepositoryStub) SaveEgressOperationsConfig(_ context.Context, value domain.OperationsConfig) (domain.OperationsConfig, error) {
	r.saved = value
	r.config = value
	r.saveCalls++
	return value, nil
}

func (r *operationsConfigRepositoryStub) UpdateEgressSourceSync(context.Context, uint64, time.Time, time.Time, int, string) error {
	r.syncUpdates++
	return nil
}

func testOperationsConfigInput() OperationsConfigInput {
	return OperationsConfigInput{
		ProbeProvider:             domain.ProbeProviderCloudflare,
		ProbeIntervalSeconds:      900,
		AssignmentIntervalSeconds: 300,
	}
}

func testEgressCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func TestUpdateOperationsConfigUsesExplicitSubscriptionProxyTriState(t *testing.T) {
	cipher := testEgressCipher(t)
	existing, err := cipher.Encrypt("socks5h://old.example:1080")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("omitted preserves existing secret", func(t *testing.T) {
		repository := &operationsConfigRepositoryStub{config: domain.DefaultOperationsConfig()}
		repository.config.EncryptedSubscriptionProxyURL = existing
		service := &Service{operations: repository, cipher: cipher}
		if _, err := service.UpdateOperationsConfig(context.Background(), testOperationsConfigInput()); err != nil {
			t.Fatal(err)
		}
		if repository.saved.EncryptedSubscriptionProxyURL != existing {
			t.Fatal("omitted subscription proxy did not preserve the existing secret")
		}
	})

	t.Run("empty value requires explicit clear", func(t *testing.T) {
		repository := &operationsConfigRepositoryStub{config: domain.DefaultOperationsConfig()}
		repository.config.EncryptedSubscriptionProxyURL = existing
		service := &Service{operations: repository, cipher: cipher}
		input := testOperationsConfigInput()
		empty := ""
		input.SubscriptionProxyURL = &empty
		_, err := service.UpdateOperationsConfig(context.Background(), input)
		if !errors.Is(err, ErrInvalidInput) || repository.saveCalls != 0 {
			t.Fatalf("empty proxy error=%v saveCalls=%d", err, repository.saveCalls)
		}
	})

	t.Run("clear removes existing secret", func(t *testing.T) {
		repository := &operationsConfigRepositoryStub{config: domain.DefaultOperationsConfig()}
		repository.config.EncryptedSubscriptionProxyURL = existing
		service := &Service{operations: repository, cipher: cipher}
		input := testOperationsConfigInput()
		input.ClearSubscriptionProxy = true
		if _, err := service.UpdateOperationsConfig(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if repository.saved.EncryptedSubscriptionProxyURL != "" {
			t.Fatal("explicit clear preserved the subscription proxy")
		}
	})

	t.Run("new value is encrypted", func(t *testing.T) {
		repository := &operationsConfigRepositoryStub{config: domain.DefaultOperationsConfig()}
		service := &Service{operations: repository, cipher: cipher}
		input := testOperationsConfigInput()
		proxyURL := "socks5h://user:secret@new.example:1080"
		input.SubscriptionProxyURL = &proxyURL
		if _, err := service.UpdateOperationsConfig(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		if repository.saved.EncryptedSubscriptionProxyURL == "" || repository.saved.EncryptedSubscriptionProxyURL == proxyURL {
			t.Fatal("new subscription proxy was not encrypted")
		}
		decrypted, err := cipher.Decrypt(repository.saved.EncryptedSubscriptionProxyURL)
		if err != nil || decrypted != proxyURL {
			t.Fatalf("decrypted proxy=%q err=%v", decrypted, err)
		}
	})

	t.Run("account placeholder is rejected", func(t *testing.T) {
		repository := &operationsConfigRepositoryStub{config: domain.DefaultOperationsConfig()}
		service := &Service{operations: repository, cipher: cipher}
		input := testOperationsConfigInput()
		proxyURL := "socks5h://Default.{account}:secret@proxy.example:1080"
		input.SubscriptionProxyURL = &proxyURL
		_, err := service.UpdateOperationsConfig(context.Background(), input)
		if !errors.Is(err, ErrInvalidInput) || repository.saveCalls != 0 {
			t.Fatalf("placeholder proxy error=%v saveCalls=%d", err, repository.saveCalls)
		}
	})
}

func TestSyncSourceFailsClosedWhenOperationsConfigCannotBeRead(t *testing.T) {
	repository := &operationsConfigRepositoryStub{getErr: errors.New("database unavailable")}
	service := &Service{operations: repository, cipher: testEgressCipher(t)}
	_, err := service.syncSource(context.Background(), repository, domain.SubscriptionSource{ID: 7, RefreshIntervalSeconds: 900})
	if !errors.Is(err, ErrSubscriptionSync) {
		t.Fatalf("sync error=%v", err)
	}
	if repository.syncUpdates != 1 {
		t.Fatalf("failure status updates=%d", repository.syncUpdates)
	}
}

func TestSubscriptionFetchProxyRejectsCorruptConfiguredSecret(t *testing.T) {
	service := &Service{cipher: testEgressCipher(t)}
	_, err := service.subscriptionFetchProxy(domain.OperationsConfig{EncryptedSubscriptionProxyURL: "not-ciphertext"})
	if err == nil {
		t.Fatal("corrupt configured subscription proxy was accepted")
	}
}
