package provider

import (
	"errors"
	"io"
	"sync"
)

// NewCompletionReadCloser reports whether a streamed body reached EOF before
// it was closed. Callers can record success only after a complete transfer,
// while still observing transport and close errors exactly once.
func NewCompletionReadCloser(body io.ReadCloser, onFinished func(error, bool)) io.ReadCloser {
	return &completionReadCloser{ReadCloser: body, onFinished: onFinished}
}

type completionReadCloser struct {
	io.ReadCloser
	onFinished func(error, bool)
	once       sync.Once
	readErr    error
	complete   bool
}

func (c *completionReadCloser) Read(buffer []byte) (int, error) {
	read, err := c.ReadCloser.Read(buffer)
	if errors.Is(err, io.EOF) {
		c.complete = true
	} else if err != nil {
		c.readErr = err
	}
	return read, err
}

func (c *completionReadCloser) Close() error {
	closeErr := c.ReadCloser.Close()
	c.once.Do(func() {
		feedbackErr := c.readErr
		if feedbackErr == nil {
			feedbackErr = closeErr
		}
		if c.onFinished != nil {
			c.onFinished(feedbackErr, c.complete)
		}
	})
	return closeErr
}
