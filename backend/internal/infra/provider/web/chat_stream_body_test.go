package web

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

type gatewayCloseCounter struct {
	closed atomic.Int32
}

func (counter *gatewayCloseCounter) Close() error {
	counter.closed.Add(1)
	return nil
}

func TestGatewayStreamBodyClosePropagates(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	counter := &gatewayCloseCounter{}
	body := &gatewayStreamBody{ReadCloser: reader, upstream: counter}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := body.closeUpstream(); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if counter.closed.Load() != 1 {
		t.Fatalf("upstream closed %d times", counter.closed.Load())
	}
	if err := body.upstreamError(context.Background(), io.ErrClosedPipe); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed stream lost cancellation: %v", err)
	}
	if _, err := writer.Write([]byte("data")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error = %v", err)
	}
}

type gatewayHealthRepository struct {
	egressRepositoryStub
	lookups atomic.Int32
}

func (repository *gatewayHealthRepository) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	repository.lookups.Add(1)
	return egressdomain.Node{}, errors.New("unexpected health lookup")
}

func TestGatewayStreamCancellationPreservesEgressHealth(t *testing.T) {
	repository := &gatewayHealthRepository{}
	adapter := &Adapter{egress: infraegress.NewManager(repository, nil)}
	source, upstreamWriter := io.Pipe()
	defer upstreamWriter.Close()
	body := adapter.streamOpenAIResponse(
		context.Background(), source, &infraegress.Lease{NodeID: 7}, account.Credential{},
		"response-test", "grok-chat-fast", "chat", "test", nil, toolConfiguration{},
		true, conversation.ResponseOptions{},
	)
	buffer := make([]byte, 4096)
	if _, err := body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	readFinished := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readFinished <- err
	}()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readFinished:
	case <-time.After(time.Second):
		t.Fatal("stream reader remained blocked")
	}
	if _, err := upstreamWriter.Write([]byte("event")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("upstream writer remains open: %v", err)
	}
	select {
	case <-body.(*gatewayStreamBody).done:
	case <-time.After(time.Second):
		t.Fatal("stream producer remained blocked")
	}
	if repository.lookups.Load() != 0 {
		t.Fatal("client close caused egress health feedback")
	}
}
